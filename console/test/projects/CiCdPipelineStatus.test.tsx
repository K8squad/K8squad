// test/projects/CiCdPipelineStatus.test.tsx — ISI-4674: the CI/CD Pipeline
// Status screen. Covers the acceptance criteria:
//   - pipeline stages + check runs rendered with status colors;
//   - run titles deep-link (mirror url preferred, actions/runs/{id} fallback);
//   - check runs link back to GitHub;
// plus the honesty rules (no fabricated status, no fabricated time).

import { describe, it, expect } from "vitest";
import { render, screen, within } from "@testing-library/react";
import {
  CiCdPipelineStatus,
  checkStatus,
  checkRunHref,
  formatBytes,
  summarize,
} from "@/components/github/CiCdPipelineStatus";
import type { GithubCheck, GithubStatus } from "@/lib/github-status";

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

const checks: GithubCheck[] = [
  { name: "build", state: "completed", conclusion: "success", url: "https://github.com/o/r/actions/runs/1/job/10" },
  { name: "test", state: "completed", conclusion: "failure", url: "https://github.com/o/r/actions/runs/1/job/11" },
  { name: "scan", state: "in_progress", url: "https://github.com/o/r/actions/runs/1/job/12" },
  { name: "deploy", state: "queued" },
];

describe("checkStatus", () => {
  it("maps conclusions and lifecycle states onto the four statuses", () => {
    expect(checkStatus({ name: "a", state: "completed", conclusion: "success" })).toBe("passed");
    expect(checkStatus({ name: "b", state: "completed", conclusion: "neutral" })).toBe("passed");
    expect(checkStatus({ name: "c", state: "completed", conclusion: "skipped" })).toBe("passed");
    expect(checkStatus({ name: "d", state: "completed", conclusion: "failure" })).toBe("failed");
    expect(checkStatus({ name: "e", state: "completed", conclusion: "timed_out" })).toBe("failed");
    expect(checkStatus({ name: "f", state: "in_progress" })).toBe("running");
    expect(checkStatus({ name: "g", state: "queued" })).toBe("pending");
    // completed-without-conclusion must NOT be guessed green.
    expect(checkStatus({ name: "h", state: "completed" })).toBe("pending");
  });
});

describe("summarize", () => {
  it("rolls check runs into counts and an honest success rate", () => {
    const s = summarize(checks);
    expect(s).toMatchObject({ total: 4, passed: 1, failed: 1, running: 1, pending: 1, completed: 2 });
    expect(s.successRate).toBe(0.5);
  });

  it("returns a null success rate when nothing has completed (no fake 0)", () => {
    const s = summarize([{ name: "queued", state: "queued" }]);
    expect(s.completed).toBe(0);
    expect(s.successRate).toBeNull();
  });
});

describe("checkRunHref", () => {
  it("prefers the mirror-normalized GitHub url", () => {
    expect(checkRunHref({ name: "ci", state: "completed", conclusion: "success", url: "https://gh/runs/9" })).toBe(
      "https://gh/runs/9",
    );
  });

  it("reconstructs actions/runs/{id} when a run id is present but the url is not", () => {
    expect(checkRunHref({ name: "ci", state: "queued", runId: 4242 })).toBe(
      "https://github.com/K8squad/K8squad/actions/runs/4242",
    );
  });

  it("falls back to the repo Actions tab when neither url nor id is known", () => {
    expect(checkRunHref({ name: "ci", state: "queued" })).toBe("https://github.com/K8squad/K8squad/actions");
  });
});

describe("formatBytes", () => {
  it("humanizes byte sizes and abstains on absent data", () => {
    expect(formatBytes(512)).toBe("512 B");
    expect(formatBytes(4096)).toBe("4.0 KB");
    expect(formatBytes(5 * 1024 * 1024)).toBe("5.0 MB");
    expect(formatBytes(undefined)).toBeNull();
  });
});

describe("CiCdPipelineStatus", () => {
  it("renders stages + check runs with status colors and GitHub deep links (ISI-4674 AC)", () => {
    render(
      <CiCdPipelineStatus
        data={status({
          checkRuns: checks,
          artifacts: [{ name: "logs", url: "https://gh/artifact/55", sizeBytes: 4096 }],
        })}
      />,
    );

    // Summary band derived from the mirror.
    expect(screen.getByTestId("gh-cicd-stat-passed").textContent).toContain("1");
    expect(screen.getByTestId("gh-cicd-stat-failed").textContent).toContain("1");
    expect(screen.getByTestId("gh-cicd-stat-running").textContent).toContain("1");
    expect(screen.getByTestId("gh-cicd-stat-pending").textContent).toContain("1");
    expect(screen.getByTestId("gh-cicd-stat-success-rate").textContent).toContain("50%");

    // Every check run is a stage with the right status color hook.
    const stages = screen.getAllByTestId("gh-cicd-stage");
    expect(stages).toHaveLength(4);
    expect(stages.map((s) => s.getAttribute("data-status"))).toEqual([
      "passed",
      "failed",
      "running",
      "pending",
    ]);

    // Check runs link back to GitHub (mirror url).
    const runRows = screen.getAllByTestId("check-row");
    expect(runRows).toHaveLength(4);
    const build = within(screen.getByTestId("panel-checks")).getByText(/^build/).closest("a");
    expect(build?.getAttribute("href")).toBe("https://github.com/o/r/actions/runs/1/job/10");
    expect(build?.getAttribute("target")).toBe("_blank");
    // The raw provider verdict is surfaced verbatim.
    expect(screen.getByText(/^success$/)).toBeTruthy();

    // Artifacts are deep-linked with a humanized size.
    expect(screen.getByTestId("panel-artifacts")).toBeTruthy();
    expect(screen.getByTestId("gh-cicd-artifact-link").getAttribute("href")).toBe("https://gh/artifact/55");
    expect(screen.getByText("4.0 KB")).toBeTruthy();
  });

  it("shows honest freshness, never a fabricated 'live' badge", () => {
    render(<CiCdPipelineStatus data={status({ checkRuns: checks })} />);
    expect(screen.getByTestId("gh-cicd-freshness").textContent).toMatch(/synced 2 min ago/);
    expect(screen.queryByText(/live/i)).toBeNull();
  });

  it("renders an honest pending status for a completed run with no conclusion", () => {
    render(<CiCdPipelineStatus data={status({ checkRuns: [{ name: "mystery", state: "completed" }] })} />);
    const row = within(screen.getByTestId("panel-checks")).getByTestId("check-row");
    expect(row.getAttribute("data-status")).toBe("pending");
    expect(row.textContent).toMatch(/pending/);
  });

  it("renders nothing when the mirror has no check runs or artifacts", () => {
    const { container } = render(<CiCdPipelineStatus data={status()} />);
    expect(container.firstChild).toBeNull();
    expect(screen.queryByTestId("gh-cicd")).toBeNull();
  });

  it("renders the artifacts panel even when only artifacts are mirrored", () => {
    render(<CiCdPipelineStatus data={status({ artifacts: [{ name: "coverage", url: "https://gh/a/1" }] })} />);
    expect(screen.getByTestId("gh-cicd")).toBeTruthy();
    expect(screen.getByTestId("panel-artifacts")).toBeTruthy();
    expect(screen.queryByTestId("panel-checks")).toBeNull();
  });

  it("ghosts last-good data instead of blanking on a stale mirror", () => {
    render(<CiCdPipelineStatus data={status({ checkRuns: checks })} ghost />);
    expect(screen.getByTestId("gh-cicd").getAttribute("data-ghost")).toBe("true");
    expect(screen.getByTestId("gh-cicd").className).toContain("gh-screen--ghost");
  });
});
