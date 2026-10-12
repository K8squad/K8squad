// test/overview/githubSummary.test.ts — pure projections behind the WS-3 GitHub
// summary strip (ISI-5708): repo-URL → mirror slug normalization and the per-repo
// open-PR lookup the project jump row keys on. No fetch, no DOM.

import { describe, it, expect } from "vitest";
import {
  repoSlug,
  openPrsByRepo,
  classifyGithubStatus,
  type GithubSummary,
} from "@/lib/overview/githubSummary";

describe("repoSlug", () => {
  it("normalizes https, scp-like, .git, and bare forms to lower-case owner/repo", () => {
    expect(repoSlug("https://github.com/Acme/WebApp.git")).toBe("acme/webapp");
    expect(repoSlug("https://github.com/acme/webapp")).toBe("acme/webapp");
    expect(repoSlug("git@github.com:acme/webapp.git")).toBe("acme/webapp");
    expect(repoSlug("github.com/acme/webapp")).toBe("acme/webapp");
    expect(repoSlug("acme/webapp")).toBe("acme/webapp");
    // Trailing slash + nested host path still resolve to the final owner/repo pair.
    expect(repoSlug("https://git.example.com/org/acme/webapp/")).toBe("acme/webapp");
  });

  it("returns null when no owner/repo pair can be derived (renders '—', never a guess)", () => {
    expect(repoSlug("")).toBeNull();
    expect(repoSlug(null)).toBeNull();
    expect(repoSlug(undefined)).toBeNull();
    expect(repoSlug("justone")).toBeNull();
  });
});

describe("openPrsByRepo", () => {
  const summary: GithubSummary = {
    totals: { issuesAssigned: 0, issuesOpen: 0, prsOpened: 0, prsReviewed: 0, prsMerged: 0, checksRed: 0 },
    mergeSparkline7d: [],
    byRepo: [
      { repo: "acme/webapp", assigned: 1, open: 2, prs: 3, reviewed: 2, checksRed: 0 },
      { repo: "acme/infra", assigned: 0, open: 0, prs: 0, reviewed: 0, checksRed: 1 },
    ],
  };
  it("maps each slug to its open-PR count (case-insensitive)", () => {
    const m = openPrsByRepo(summary);
    expect(m.get("acme/webapp")).toBe(3);
    expect(m.get("acme/infra")).toBe(0);
    expect(m.get(repoSlug("https://github.com/Acme/WebApp.git")!)).toBe(3);
  });
  it("is empty for a null summary", () => {
    expect(openPrsByRepo(null).size).toBe(0);
  });
});

describe("classifyGithubStatus", () => {
  it("treats 404/501 as degrade and everything else as a retryable error", () => {
    expect(classifyGithubStatus(404)).toEqual({ kind: "not-available", status: 404 });
    expect(classifyGithubStatus(501)).toEqual({ kind: "not-available", status: 501 });
    expect(classifyGithubStatus(500)).toEqual({ kind: "error", status: 500 });
  });
});
