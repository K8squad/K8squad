// test/overview/githubSummary.test.ts — the WS-1b FE projections behind the project overview's
// real Open-PRs count + GitHub strip (ISI-5709 WS-4). Pure: no fetch, no DOM.

import { describe, it, expect } from "vitest";
import {
  repoSlug,
  selectRepoStat,
  repoCount,
  UNAVAILABLE,
  type GithubSummary,
  type GithubRepoStat,
} from "@/lib/overview/githubSummary";

describe("repoSlug", () => {
  it("normalizes https github urls to a lower-cased owner/repo slug", () => {
    expect(repoSlug("https://github.com/K8squad/K8squad")).toBe("k8squad/k8squad");
    expect(repoSlug("https://github.com/K8squad/K8squad.git")).toBe("k8squad/k8squad");
    expect(repoSlug("https://github.com/Acme/Web-App/")).toBe("acme/web-app");
  });

  it("handles scp-form and bare owner/repo", () => {
    expect(repoSlug("git@github.com:Acme/Web.git")).toBe("acme/web");
    expect(repoSlug("Acme/Web")).toBe("acme/web");
  });

  it("returns null for anything that is not exactly owner/repo", () => {
    expect(repoSlug(null)).toBeNull();
    expect(repoSlug("")).toBeNull();
    expect(repoSlug("   ")).toBeNull();
    expect(repoSlug("https://github.com/OnlyOwner")).toBeNull();
    expect(repoSlug("https://github.com/a/b/c")).toBeNull();
  });
});

const summary: GithubSummary = {
  totals: {
    issuesAssigned: 3,
    issuesOpen: 10,
    prsOpened: 4,
    prsReviewed: 2,
    prsMerged: 7,
    checksRed: 1,
  },
  mergeSparkline7d: [0, 1, 0, 2, 1, 0, 3],
  byRepo: [
    { repo: "k8squad/k8squad", assigned: 2, open: 6, prs: 4, reviewed: 2, checksRed: 1 },
    { repo: "acme/web", assigned: 1, open: 4, prs: 0, reviewed: 0, checksRed: 0 },
  ],
};

describe("selectRepoStat", () => {
  it("matches the byRepo row for a project's repoUrl case-insensitively", () => {
    const stat = selectRepoStat(summary, "https://github.com/K8squad/K8squad");
    expect(stat?.prs).toBe(4);
    expect(stat?.checksRed).toBe(1);
  });

  it("returns null when the repo is not mirrored or the url is unparseable", () => {
    expect(selectRepoStat(summary, "https://github.com/other/repo")).toBeNull();
    expect(selectRepoStat(summary, "not-a-url")).toBeNull();
    expect(selectRepoStat(null, "https://github.com/K8squad/K8squad")).toBeNull();
  });
});

describe("repoCount (FR-I3)", () => {
  const stat: GithubRepoStat = summary.byRepo[1]; // acme/web, prs: 0
  it("renders an authoritative zero as 0, not '—'", () => {
    expect(repoCount(stat, "prs")).toBe(0);
  });
  it("renders '—' when the repo row is absent (unavailable, not fabricated 0)", () => {
    expect(repoCount(null, "prs")).toBe(UNAVAILABLE);
  });
});
