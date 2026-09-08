// test/projects/GitHubStatusTab.test.tsx — ISI-3956 S5c: the GitHub-status tab.
// Covers AC1 panels (link out), AC2 honest freshness ("synced Ns ago"),
// AC5 loading/empty/501 honest states, and no-credential-in-DOM (AC6).

import { describe, it, expect, afterEach, vi } from "vitest";
import { render, screen, cleanup, waitFor } from "@testing-library/react";
import { GitHubStatusTab } from "@/components/GitHubStatusTab";
import type { GithubStatus } from "@/lib/github-status";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

function stub(status: number, body?: unknown) {
  const spy = vi.fn(() =>
    Promise.resolve({
      ok: status >= 200 && status < 300,
      status,
      json: () => Promise.resolve(body ?? null),
      text: () => Promise.resolve(JSON.stringify(body ?? null)),
    } as Response),
  );
  vi.stubGlobal("fetch", spy);
  return spy;
}

const projection: GithubStatus = {
  project: { name: "web", namespace: "squad-a" },
  pullRequests: [
    { number: 7, title: "add feature", state: "open", reviewState: "ready-for-review", branch: "feat/x", url: "https://gh/pull/7" },
    { number: 8, title: "shipped", state: "closed", reviewState: "merged", merged: true, url: "https://gh/pull/8" },
  ],
  issues: [{ number: 3, title: "a bug", state: "open", url: "https://gh/issues/3" }],
  checkRuns: [{ name: "ci", state: "completed", conclusion: "success", url: "https://gh/runs/101" }],
  artifacts: [{ name: "logs", url: "https://gh/artifact/55", sizeBytes: 4096 }],
  releases: [{ name: "v1.0.0", tag: "v1.0.0", state: "published", url: "https://gh/releases/v1.0.0" }],
  branches: [
    { name: "main", default: true, headSha: "abc1234def5678", url: "https://gh/tree/main" },
    { name: "feat/x", headSha: "fff2345fff5678" },
  ],
  freshness: {
    lastMirrorTime: new Date(Date.now() - 5_000).toISOString(),
    lastWebhookTime: new Date(Date.now() - 5_000).toISOString(),
    mirrorRecordCount: 6,
  },
};

describe("GitHubStatusTab", () => {
  it("renders every panel from a mirror projection, linking out (AC1)", async () => {
    stub(200, projection);
    render(<GitHubStatusTab projectId="web" />);

    await waitFor(() => expect(screen.getByTestId("github-status")).toBeTruthy());
    expect(screen.getByTestId("panel-prs")).toBeTruthy();
    expect(screen.getByTestId("panel-issues")).toBeTruthy();
    expect(screen.getByTestId("panel-checks")).toBeTruthy();
    expect(screen.getByTestId("panel-artifacts")).toBeTruthy();
    expect(screen.getByTestId("panel-releases")).toBeTruthy();
    expect(screen.getByTestId("panel-branches")).toBeTruthy();

    // PRs both rendered; open shows review state, merged shows merged.
    expect(screen.getAllByTestId("pr-row")).toHaveLength(2);
    expect(screen.getByText(/ready-for-review/)).toBeTruthy();
    expect(screen.getByText(/merged/)).toBeTruthy();
    // Check conclusion surfaced.
    expect(screen.getByText(/success/)).toBeTruthy();

    // Links point at the normalized GitHub url.
    const pr7 = screen.getByText(/#7 add feature/).closest("a");
    expect(pr7?.getAttribute("href")).toBe("https://gh/pull/7");

    // Branches: default badge + short head SHA, default branch links out.
    expect(screen.getAllByTestId("branch-row")).toHaveLength(2);
    const main = screen.getByText(/main/).closest("a");
    expect(main?.getAttribute("href")).toBe("https://gh/tree/main");
    expect(screen.getByText(/default/)).toBeTruthy();
    expect(screen.getByText(/abc1234/)).toBeTruthy();
  });

  it("hides the branch panel when the mirror has no branch rows", async () => {
    stub(200, { ...projection, branches: [] });
    render(<GitHubStatusTab projectId="web" />);
    await waitFor(() => expect(screen.getByTestId("github-status")).toBeTruthy());
    expect(screen.queryByTestId("panel-branches")).toBeNull();
  });

  it("shows honest freshness 'synced Ns ago' from timestamps, not a live badge (AC2)", async () => {
    stub(200, projection);
    render(<GitHubStatusTab projectId="web" />);
    await waitFor(() => expect(screen.getByTestId("github-freshness")).toBeTruthy());
    expect(screen.getByTestId("github-freshness").textContent).toMatch(/synced .*ago/);
    expect(screen.queryByText(/live/i)).toBeNull();
  });

  it("never leaks a credential/secret/token into the DOM (AC6)", async () => {
    stub(200, projection);
    const { container } = render(<GitHubStatusTab projectId="web" />);
    await waitFor(() => expect(screen.getByTestId("github-status")).toBeTruthy());
    const html = container.innerHTML.toLowerCase();
    for (const forbidden of ["credential", "secret", "token", "password"]) {
      expect(html.includes(forbidden)).toBe(false);
    }
  });

  it("renders the empty state for an empty mirror (AC5)", async () => {
    stub(200, {
      project: { name: "web", namespace: "squad-a" },
      pullRequests: [],
      issues: [],
      checkRuns: [],
      artifacts: [],
      releases: [],
      branches: [],
      freshness: { mirrorRecordCount: 0 },
    });
    render(<GitHubStatusTab projectId="web" />);
    await waitFor(() => expect(screen.getByTestId("github-empty")).toBeTruthy());
    expect(screen.getByText(/No GitHub activity yet/)).toBeTruthy();
  });

  it("renders an honest 'not available yet' on 501 (AC5)", async () => {
    stub(501, null);
    render(<GitHubStatusTab projectId="web" />);
    await waitFor(() => expect(screen.getByTestId("github-honest")).toBeTruthy());
    expect(screen.getByText(/not available yet/)).toBeTruthy();
  });

  it("renders an existence-hiding not-found on 404", async () => {
    stub(404, null);
    render(<GitHubStatusTab projectId="web" />);
    await waitFor(() => expect(screen.getByTestId("github-honest")).toBeTruthy());
    expect(screen.getByText(/No GitHub status/)).toBeTruthy();
  });
});
