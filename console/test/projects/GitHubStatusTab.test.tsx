// test/projects/GitHubStatusTab.test.tsx — ISI-3956 S5c: the GitHub-status tab.
// Covers AC1 panels (link out), AC2 honest freshness ("synced Ns ago"),
// AC5 loading/empty/501 honest states, and no-credential-in-DOM (AC6).

import { describe, it, expect, afterEach, vi } from "vitest";
import { render, screen, cleanup, waitFor, within, fireEvent } from "@testing-library/react";
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
  sync: { reason: "Synced", trigger: "webhook", ageSeconds: 120 },
};

describe("GitHubStatusTab", () => {
  it("renders every panel from a mirror projection, linking out (AC1)", async () => {
    stub(200, projection);
    render(<GitHubStatusTab projectId="web" />);

    await waitFor(() => expect(screen.getByTestId("github-status")).toBeTruthy());
    expect(screen.getByTestId("panel-prs")).toBeTruthy();
    // The issues panel lists the mirror's issues, each deep-linked.
    expect(screen.getByTestId("panel-issues")).toBeTruthy();
    expect(screen.getByTestId("panel-checks")).toBeTruthy();
    expect(screen.getByTestId("panel-artifacts")).toBeTruthy();
    expect(screen.getByTestId("panel-releases")).toBeTruthy();
    expect(screen.getByTestId("panel-branches")).toBeTruthy();

    // PRs both rendered; the Pull Request Management screen shows the raw
    // review state on each card (ISI-4672).
    expect(screen.getAllByTestId("pr-row")).toHaveLength(2);
    // Scoped to the PR panel so it can't collide with the overview timeline/badges.
    const prPanel = within(screen.getByTestId("panel-prs"));
    expect(prPanel.getByText(/ready-for-review/)).toBeTruthy();
    expect(prPanel.getByText(/merged/)).toBeTruthy();
    // Check conclusion surfaced (scoped to the CI/CD panel: the overview's
    // "CI success rate" label also matches an unscoped /success/ query).
    expect(within(screen.getByTestId("panel-checks")).getByText(/success/)).toBeTruthy();

    // Each PR title deep-links to the normalized GitHub url via `pull/{n}`.
    const pr7 = screen.getAllByTestId("gh-pr-link")[0];
    expect(pr7.getAttribute("href")).toBe("https://gh/pull/7");
    expect(pr7.textContent).toMatch(/#7: add feature/);

    // Branches: default badge + short head SHA, default branch links out.
    // Scoped to the branch panel (other screens also mention "main"/"default").
    const branchPanel = within(screen.getByTestId("panel-branches"));
    expect(branchPanel.getAllByTestId("branch-row")).toHaveLength(2);
    const main = branchPanel.getByText(/main/).closest("a");
    expect(main?.getAttribute("href")).toBe("https://gh/tree/main");
    expect(branchPanel.getByText(/default/)).toBeTruthy();
    expect(branchPanel.getByText(/abc1234/)).toBeTruthy();
  });

  it("renders the CI/CD Pipeline Status screen with linked check runs (ISI-4674)", async () => {
    stub(200, projection);
    render(<GitHubStatusTab projectId="web" />);
    await waitFor(() => expect(screen.getByTestId("gh-cicd")).toBeTruthy());

    // Summary band + stage flow derived from the one mirrored check run.
    expect(screen.getByTestId("gh-cicd-summary").textContent).toMatch(/Success rate/);
    const stage = screen.getByTestId("gh-cicd-stage");
    expect(stage.getAttribute("data-status")).toBe("passed");
    expect(stage.textContent).toMatch(/ci/);

    // The run title deep-links back to GitHub (mirror-normalized url).
    const link = screen.getByTestId("gh-cicd-run-link");
    expect(link.getAttribute("href")).toBe("https://gh/runs/101");
    expect(link.getAttribute("target")).toBe("_blank");
    expect(link.textContent).toMatch(/↗/);
  });

  it("renders the Overview Dashboard: metrics, timeline, repo health (ISI-4671)", async () => {
    stub(200, projection);
    render(<GitHubStatusTab projectId="web" />);
    await waitFor(() => expect(screen.getByTestId("github-overview")).toBeTruthy());

    // 5-up activity metric band derived from the mirror rows.
    expect(screen.getByTestId("stat-branches").textContent).toContain("2");
    expect(screen.getByTestId("stat-open-prs").textContent).toContain("1");
    expect(screen.getByTestId("stat-open-issues").textContent).toContain("1");
    expect(screen.getByTestId("stat-check-runs").textContent).toContain("1");
    expect(screen.getByTestId("stat-releases").textContent).toContain("1");
    // Open/merged/closed breakdown on the PR tile.
    expect(screen.getByTestId("stat-open-prs").textContent).toMatch(/1 merged · 0 closed/);

    // Recent-activity timeline: one row per PR/issue/release, each deep-linked.
    // Scoped to the timeline so it can't collide with the entity panels below.
    const timeline = within(screen.getByTestId("github-timeline"));
    expect(timeline.getAllByTestId("github-timeline-row")).toHaveLength(4);
    expect(timeline.getByText(/PR #8: shipped/).closest("a")?.getAttribute("href")).toBe(
      "https://gh/pull/8",
    );
    expect(timeline.getByText(/Issue #3: a bug/).closest("a")?.getAttribute("href")).toBe(
      "https://gh/issues/3",
    );
    expect(timeline.getByText(/v1\.0\.0/).closest("a")?.getAttribute("href")).toBe(
      "https://gh/releases/v1.0.0",
    );

    // Repo health is derived honestly: merged/closed (100%), checks (100%), issue close (0%).
    expect(screen.getByTestId("github-health")).toBeTruthy();
    expect(screen.getByTestId("health-score").textContent).toBe("67%");
    expect(screen.getByTestId("health-tier").textContent).toBe("Fair");
  });

  it("header + repo slug deep-link to GitHub (ISI-4671)", async () => {
    stub(200, projection);
    render(<GitHubStatusTab projectId="web" />);
    await waitFor(() => expect(screen.getByTestId("github-repo-link")).toBeTruthy());
    expect(screen.getByTestId("github-repo-link").getAttribute("href")).toBe(
      "https://github.com/K8squad/K8squad",
    );
    expect(screen.getByTestId("github-open-github").getAttribute("href")).toBe(
      "https://github.com/K8squad/K8squad",
    );
    // The repo slug is rendered, and the external-link glyph is decorative.
    expect(screen.getByTestId("github-repo-link").textContent).toMatch(/K8squad\/K8squad/);
  });

  it("degrades repo health to — (never a fabricated 0) when no signal is derivable", async () => {
    stub(200, {
      ...projection,
      // A single open PR, no issues, no completed checks: no denominator anywhere.
      pullRequests: [
        { number: 1, title: "wip", state: "open", url: "https://gh/pull/1" },
      ],
      issues: [],
      checkRuns: [],
      releases: [],
      branches: [],
    });
    render(<GitHubStatusTab projectId="web" />);
    await waitFor(() => expect(screen.getByTestId("github-health")).toBeTruthy());
    expect(screen.getByTestId("health-score").textContent).toBe("—");
    expect(screen.getByTestId("health-tier").textContent).toBe("Not enough data");
  });

  it("hides the branch panel when the mirror has no branch rows", async () => {
    stub(200, { ...projection, branches: [] });
    render(<GitHubStatusTab projectId="web" />);
    await waitFor(() => expect(screen.getByTestId("github-status")).toBeTruthy());
    expect(screen.queryByTestId("panel-branches")).toBeNull();
  });

  it("shows honest freshness 'Synced · N ago · via <trigger>' from the mirror, not a live badge (AC2)", async () => {
    stub(200, projection);
    render(<GitHubStatusTab projectId="web" />);
    await waitFor(() => expect(screen.getByTestId("github-freshness")).toBeTruthy());
    const chip = screen.getByTestId("github-freshness").textContent ?? "";
    expect(chip).toMatch(/Synced · .*ago/);
    // Trigger source proves the AC "no manual kick" — refresh is automatic.
    expect(chip).toMatch(/via webhook/);
    expect(screen.getByTestId("github-autoline").textContent).toMatch(/no manual kick/);
    // A green (running) chip; never a fabricated "live" badge.
    expect(screen.getByTestId("github-chip").getAttribute("data-tone")).toBe("running");
    expect(screen.queryByText(/live/i)).toBeNull();
  });

  it("drives the chip green→amber→red machine off sync.reason (DESIGN-SPEC §2)", async () => {
    // CredentialMissing ⇒ red "blocked" chip + reconnect state card, last-good
    // data kept ghosted (degrade, don't blank).
    stub(200, {
      ...projection,
      sync: { reason: "CredentialMissing", trigger: "poll", ageSeconds: 900 },
    });
    render(<GitHubStatusTab projectId="web" />);
    await waitFor(() => expect(screen.getByTestId("github-chip")).toBeTruthy());
    expect(screen.getByTestId("github-chip").getAttribute("data-tone")).toBe("blocked");
    expect(screen.getByTestId("github-chip").textContent).toMatch(/reconnect required/);
    const cardCred = screen.getByTestId("github-state-card");
    expect(cardCred.getAttribute("data-reason")).toBe("CredentialMissing");
    expect(cardCred.textContent).toMatch(/GitHub token can't be resolved/);
    // Degrade, don't blank: panels remain, marked ghosted.
    expect(screen.getByTestId("github-panels").getAttribute("data-ghost")).toBe("true");
  });

  it("renders the ProviderError card amber with auto-retry copy, keeping last-good data", async () => {
    stub(200, {
      ...projection,
      sync: { reason: "ProviderError", trigger: "poll", ageSeconds: 840 },
    });
    render(<GitHubStatusTab projectId="web" />);
    await waitFor(() => expect(screen.getByTestId("github-state-card")).toBeTruthy());
    expect(screen.getByTestId("github-chip").getAttribute("data-tone")).toBe("paused");
    expect(screen.getByTestId("github-chip").textContent).toMatch(/last good/);
    expect(screen.getByTestId("github-state-card").textContent).toMatch(/GitHub unreachable — retrying/);
    expect(screen.getByTestId("github-panels").getAttribute("data-ghost")).toBe("true");
  });

  it("surfaces the freshness SLI: Synced but past the SLO ⇒ amber 'Data is behind schedule'", async () => {
    stub(200, {
      ...projection,
      sync: { reason: "Synced", trigger: "poll", ageSeconds: 600 }, // > 360 SLO
    });
    render(<GitHubStatusTab projectId="web" />);
    await waitFor(() => expect(screen.getByTestId("github-state-card")).toBeTruthy());
    expect(screen.getByTestId("github-chip").getAttribute("data-tone")).toBe("paused");
    expect(screen.getByTestId("github-state-card").textContent).toMatch(/Data is behind schedule/);
  });

  it("renders the first-run 'No repository linked' card for SyncNotConfigured (no chip)", async () => {
    stub(200, {
      project: { name: "web", namespace: "squad-a" },
      pullRequests: [], issues: [], checkRuns: [], artifacts: [], releases: [], branches: [],
      freshness: { mirrorRecordCount: 0 },
      sync: { reason: "SyncNotConfigured" },
    });
    render(<GitHubStatusTab projectId="web" />);
    await waitFor(() => expect(screen.getByTestId("github-state-card")).toBeTruthy());
    expect(screen.queryByTestId("github-chip")).toBeNull();
    expect(screen.getByTestId("github-state-card").textContent).toMatch(/No repository linked/);
    // CTA is a link into project settings to link the repo.
    expect(screen.getByTestId("github-state-card-cta").getAttribute("href")).toMatch(/\/settings$/);
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
      sync: { reason: "Synced", trigger: "poll", ageSeconds: 30 },
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

  // ---------------------------------------------------------------------------
  // ISI-4675 — Releases & Branches screen.
  // ---------------------------------------------------------------------------

  it("renders the Releases & Branches screen with tags, notes and stats (ISI-4675)", async () => {
    stub(200, {
      ...projection,
      releases: [
        {
          name: "Performance improvements & bug fixes",
          tag: "v2.1.0",
          state: "published",
          actor: "mikelee",
          publishedAt: new Date(Date.now() - 2 * 86_400_000).toISOString(),
        },
        { name: "v2.0.8", tag: "v2.0.8", state: "prerelease" },
      ],
      branches: [
        { name: "main", default: true, headSha: "abc1234def5678" },
        { name: "feat/x", headSha: "fff2345fff5678" },
      ],
    });
    render(<GitHubStatusTab projectId="web" />);
    await waitFor(() => expect(screen.getByTestId("github-tabs")).toBeTruthy());

    // Defaults to the sync-health view; the releases screen is behind its tab.
    expect(screen.getByTestId("github-tab-status").getAttribute("aria-selected")).toBe("true");
    fireEvent.click(screen.getByTestId("github-tab-releases"));

    expect(screen.getByTestId("gh-releases-branches")).toBeTruthy();
    expect(screen.getByTestId("github-tab-releases").getAttribute("aria-selected")).toBe("true");

    // Release timeline: tag + notes + honest recency, state badge.
    expect(screen.getAllByTestId("gh-release-row")).toHaveLength(2);
    const firstTag = screen.getAllByTestId("gh-release-link")[0];
    expect(firstTag.textContent).toMatch(/v2\.1\.0/);
    expect(screen.getByText("Performance improvements & bug fixes")).toBeTruthy();
    expect(screen.getByText(/Released .* ago by @mikelee/)).toBeTruthy();

    // Honest stats from the mirror (no fabricated contributor counts).
    expect(screen.getByTestId("gh-stat-total-releases").textContent).toMatch(/2/);
    expect(screen.getByTestId("gh-stat-published").textContent).toMatch(/1/);

    // Honest freshness, never a fabricated "live" badge.
    expect(screen.getByTestId("gh-freshness").textContent).toMatch(/synced/);
    expect(screen.queryByText(/live/i)).toBeNull();
  });

  it("deep-links releases via releases/tag/{tag} and branches via tree/{branch}", async () => {
    stub(200, {
      ...projection,
      // No mirrored url ⇒ the href must be reconstructed from the identifier.
      releases: [{ name: "v2.1.0", tag: "v2.1.0", state: "published" }],
      branches: [
        { name: "main", default: true, headSha: "abc1234def5678" },
        { name: "feat/x", headSha: "fff2345fff5678" },
      ],
    });
    render(<GitHubStatusTab projectId="web" />);
    await waitFor(() => expect(screen.getByTestId("github-tabs")).toBeTruthy());
    fireEvent.click(screen.getByTestId("github-tab-releases"));

    expect(screen.getByTestId("gh-release-link").getAttribute("href")).toBe(
      "https://github.com/K8squad/K8squad/releases/tag/v2.1.0",
    );

    const branchHrefs = screen
      .getAllByTestId("gh-branch-link")
      .map((a) => a.getAttribute("href"));
    expect(branchHrefs).toContain("https://github.com/K8squad/K8squad/tree/main");
    expect(branchHrefs).toContain("https://github.com/K8squad/K8squad/tree/feat/x");

    // Header deep-links to the repo.
    expect(screen.getByTestId("gh-open-github").getAttribute("href")).toBe(
      "https://github.com/K8squad/K8squad",
    );
  });

  it("keeps last-good release/branch data ghosted when the mirror is paused", async () => {
    stub(200, {
      ...projection,
      sync: { reason: "CredentialMissing", trigger: "poll", ageSeconds: 900 },
    });
    render(<GitHubStatusTab projectId="web" />);
    await waitFor(() => expect(screen.getByTestId("github-tabs")).toBeTruthy());
    fireEvent.click(screen.getByTestId("github-tab-releases"));
    expect(screen.getByTestId("gh-releases-branches").getAttribute("data-ghost")).toBe("true");
  });

  it("shows honest empty states on the releases screen", async () => {
    stub(200, { ...projection, releases: [], branches: [] });
    render(<GitHubStatusTab projectId="web" />);
    await waitFor(() => expect(screen.getByTestId("github-tabs")).toBeTruthy());
    fireEvent.click(screen.getByTestId("github-tab-releases"));
    expect(screen.getByTestId("gh-releases-empty")).toBeTruthy();
    expect(screen.getByTestId("gh-branches-empty")).toBeTruthy();
  });
});
