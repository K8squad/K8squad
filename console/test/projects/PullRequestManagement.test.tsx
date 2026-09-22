// test/projects/PullRequestManagement.test.tsx — ISI-4672: the Pull Request
// Management screen. Covers the acceptance criteria:
//   - AC1: PR cards show branch, checks and review state;
//   - AC2: each PR title deep-links via `pull/{n}` (mirror url preferred,
//     canonical `pull/{n}` reconstructed when the mirror url is absent);
//   - AC3: the workflow reflects merge state (open → in review → merged/closed);
// plus the honesty rules (no fabricated reviewers/agents/comments, no invented
// time, no fabricated merge-queue position).

import { describe, it, expect } from "vitest";
import { render, screen, within } from "@testing-library/react";
import {
  PullRequestManagement,
  checkTone,
  prStage,
  stageCounts,
} from "@/components/github/PullRequestManagement";
import type { GithubPR, GithubStatus } from "@/lib/github-status";

function status(overrides: Partial<GithubStatus> = {}): GithubStatus {
  return {
    project: { name: "web", namespace: "squad-a" },
    pullRequests: [],
    issues: [],
    checkRuns: [],
    artifacts: [],
    releases: [],
    branches: [],
    freshness: { mirrorRecordCount: 1 },
    sync: { reason: "Synced", trigger: "webhook", ageSeconds: 120 },
    ...overrides,
  };
}

const prs: GithubPR[] = [
  {
    number: 142,
    title: "feat: Add k8squad agent integration",
    state: "open",
    reviewState: "ready-for-review",
    branch: "feat/integration",
    url: "https://gh/pull/142",
    actor: "alexchen",
    updatedAt: new Date(Date.now() - 7_200_000).toISOString(),
  },
  {
    number: 143,
    title: "fix: Memory leak in agent pool",
    state: "open",
    reviewState: "ready-for-review",
    // No branch and no url on this mirror row — both must degrade honestly.
    actor: "jwilson",
  },
  { number: 141, title: "perf: Optimize CI pipeline", state: "closed", reviewState: "merged", merged: true, url: "https://gh/pull/141" },
  { number: 140, title: "chore: drop old job", state: "closed" },
];

const checks: GithubStatus["checkRuns"] = [
  { name: "build", state: "completed", conclusion: "success" },
  { name: "lint", state: "completed", conclusion: "failure" },
];

describe("prStage / stageCounts", () => {
  it("maps mirror state + reviewState onto the merge pipeline", () => {
    expect(prStage({ number: 1, title: "a", state: "open" })).toBe("open");
    expect(prStage({ number: 2, title: "b", state: "open", reviewState: "ready-for-review" })).toBe("review");
    expect(prStage({ number: 3, title: "c", state: "closed", merged: true })).toBe("merged");
    expect(prStage({ number: 4, title: "d", state: "closed", reviewState: "merged" })).toBe("merged");
    expect(prStage({ number: 5, title: "e", state: "closed" })).toBe("closed");
  });

  it("counts every stage, including honest zeros", () => {
    const counts = stageCounts([
      { number: 1, title: "a", state: "open" },
      { number: 2, title: "b", state: "open", reviewState: "ready-for-review" },
      { number: 3, title: "c", state: "closed", merged: true },
    ]);
    expect(counts).toEqual([
      { stage: "open", count: 1 },
      { stage: "review", count: 1 },
      { stage: "merged", count: 1 },
      { stage: "closed", count: 0 },
    ]);
  });
});

describe("checkTone", () => {
  it("never guesses a completed-without-conclusion run green", () => {
    expect(checkTone("completed", "success")).toBe("passed");
    expect(checkTone("completed", "failure")).toBe("failed");
    expect(checkTone("in_progress", undefined)).toBe("running");
    expect(checkTone("completed", undefined)).toBe("pending");
  });
});

describe("PullRequestManagement", () => {
  it("renders PR cards with branch, checks and review state (AC1)", () => {
    render(<PullRequestManagement data={status({ pullRequests: prs, checkRuns: checks })} />);

    const cards = screen.getAllByTestId("pr-row");
    expect(cards).toHaveLength(4);

    const first = within(cards[0]);
    expect(first.getByTestId("gh-pr-branch").textContent).toBe("feat/integration");
    expect(first.getByTestId("gh-pr-review-state").textContent).toBe("ready-for-review");
    expect(first.getAllByTestId("gh-pr-check")).toHaveLength(2);
    expect(first.getByText("build")).toBeTruthy();
    expect(first.getByText("lint")).toBeTruthy();

    // The second mirror row carries no branch — degrade honestly, don't invent.
    expect(within(cards[1]).getByTestId("gh-pr-branch").textContent).toBe("not mirrored");
  });

  it("deep-links every PR title via pull/{n} (AC2)", () => {
    render(<PullRequestManagement data={status({ pullRequests: prs })} />);
    const links = screen.getAllByTestId("gh-pr-link");

    // Mirror url preferred when present; canonical path reconstructed otherwise.
    expect(links[0].getAttribute("href")).toBe("https://gh/pull/142");
    expect(links[1].getAttribute("href")).toBe("https://github.com/K8squad/K8squad/pull/143");
    expect(links[0].getAttribute("target")).toBe("_blank");
    expect(links[0].textContent).toMatch(/#142: feat: Add k8squad agent integration/);
  });

  it("reflects merge state across the four workflow stages (AC3)", () => {
    render(<PullRequestManagement data={status({ pullRequests: prs })} />);
    const stages = screen.getAllByTestId("gh-pr-stage");
    expect(stages.map((s) => s.getAttribute("data-stage"))).toEqual([
      "open",
      "review",
      "merged",
      "closed",
    ]);
    const counts = screen.getAllByTestId("gh-pr-stage-count").map((c) => c.textContent);
    expect(counts).toEqual(["0", "2", "1", "1"]);
  });

  it("deep-links the action row to real GitHub actions", () => {
    render(<PullRequestManagement data={status({ pullRequests: prs })} />);
    const actions = within(screen.getByTestId("gh-pr-actions"));
    expect(actions.getByText(/New Pull Request/).closest("a")?.getAttribute("href")).toBe(
      "https://github.com/K8squad/K8squad/pulls/new",
    );
    expect(actions.getByText(/Drafts/).closest("a")?.getAttribute("href")).toContain("is%3Adraft");
    expect(actions.getByText(/Filters/).closest("a")?.getAttribute("href")).toContain("is%3Aopen");
  });

  it("states the mirror's limits instead of fabricating reviewers/agents/comments", () => {
    const { container } = render(
      <PullRequestManagement data={status({ pullRequests: prs, checkRuns: checks })} />,
    );
    // The mockup shows reviewers, agent tags, comments, +/- and related work
    // items — the mirror carries none of them, so they must not appear.
    for (const forbidden of [/reviewer/i, /related:/i, /agent$/i, /opened \d/i]) {
      expect(screen.queryByText(forbidden)).toBeNull();
    }
    expect(container.textContent).not.toMatch(/merge queue/i);
    // But it explains why those regions are absent.
    expect(screen.getByTestId("gh-pr-workflow-note").textContent).toMatch(/does not carry/);
  });

  it("shows the selected PR's real fields in the detail panel", () => {
    render(<PullRequestManagement data={status({ pullRequests: prs, checkRuns: checks })} />);
    const detail = within(screen.getByTestId("gh-pr-detail"));
    expect(detail.getByText(/PR #142: Detailed View/)).toBeTruthy();
    expect(detail.getByText("@alexchen")).toBeTruthy();
    expect(detail.getByTestId("gh-pr-detail-link").getAttribute("href")).toBe("https://gh/pull/142");
  });

  it("renders nothing when the mirror has no pull requests", () => {
    const { container } = render(<PullRequestManagement data={status()} />);
    expect(container.firstChild).toBeNull();
  });

  it("shows the Review automation settings button only with a projectId (ISI-4764)", () => {
    // Without a projectId the settings button is hidden (no dialog target).
    const { rerender } = render(<PullRequestManagement data={status({ pullRequests: prs })} />);
    expect(screen.queryByTestId("gh-review-automation-btn")).toBeNull();

    // With a projectId the button appears and the dialog is closed until clicked.
    rerender(<PullRequestManagement data={status({ pullRequests: prs })} projectId="ns/demo" />);
    expect(screen.getByTestId("gh-review-automation-btn")).toBeTruthy();
    expect(screen.queryByTestId("gh-review-automation-dialog")).toBeNull();
  });
});
