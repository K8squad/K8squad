// components/github/CiCdPipelineStatus.tsx — ISI-4674: the "CI/CD Pipeline
// Status" GitHub screen (child of ISI-4662, "Github screen").
//
// Renders the scm-mirror projection's check runs + artifacts to match the
// board-approved "CI/CD Pipeline Status" mockup: a status summary, a pipeline
// stage flow, a recent-check-run history, and the artifact references — in the
// IsItObservable dark palette.
//
// HONESTY (ADR-0013 §D4, carried from GitHubStatusTab): the mirror carries ONLY
// a check run's name, state, conclusion and normalized GitHub url. It carries
// no workflow-run id, stage grouping, branch or per-run timestamp, so this
// screen never invents them:
//   - "pipeline stages" are the mirrored check runs, drawn left→right;
//   - "run history" is the mirror's own check-run order (no fake timestamps);
//   - recency is the mirror's honest `synced X ago` freshness, shown once.
// Every check run and artifact deep-links back to GitHub (the mirror url when
// present, else the canonical `actions/runs/{id}` reconstruction).

import type { GithubArtifact, GithubCheck, GithubStatus } from "@/lib/github-status";
import { ageLabel } from "@/lib/github-status";
import { GITHUB_REPO_LABEL, GITHUB_REPO_URL, githubRunHref } from "@/lib/github-links";
import "./github-screens.css";
import "./cicd.css";

/** A check-run's four renderable statuses. Derived from state + conclusion —
 * never from anything the mirror does not hold. */
export type CiStatus = "passed" | "failed" | "running" | "pending";

const PASS_CONCLUSIONS = new Set(["success", "neutral", "skipped"]);
const FAIL_CONCLUSIONS = new Set([
  "failure",
  "error",
  "timed_out",
  "cancelled",
  "action_required",
  "startup_failure",
  "stale",
]);

/** GitHub check-run lifecycle → the screen's status. An in-progress run is
 * "running"; a completed run's verdict is its conclusion; anything still
 * queued/requested/unknown is honestly "pending". A completed run with no
 * conclusion is left "pending" rather than guessed green. */
export function checkStatus(c: GithubCheck): CiStatus {
  const state = (c.state || "").toLowerCase();
  const conclusion = (c.conclusion || "").toLowerCase();
  if (conclusion) return PASS_CONCLUSIONS.has(conclusion) ? "passed" : "failed";
  if (state === "in_progress" || state === "running") return "running";
  // completed-without-conclusion, queued, requested, waiting, or anything
  // unrecognized: pending is the honest default.
  return "pending";
}

const STATUS_LABEL: Record<CiStatus, string> = {
  passed: "passed",
  failed: "failed",
  running: "running",
  pending: "pending",
};

export type CiSummary = {
  total: number;
  passed: number;
  failed: number;
  running: number;
  pending: number;
  completed: number;
  /** passed/(passed+failed), or null when nothing has completed yet. */
  successRate: number | null;
};

/** Roll the mirrored check runs up into the summary band's counts. */
export function summarize(checks: GithubCheck[]): CiSummary {
  const counts: Record<CiStatus, number> = { passed: 0, failed: 0, running: 0, pending: 0 };
  for (const c of checks) counts[checkStatus(c)] += 1;
  const completed = counts.passed + counts.failed;
  return {
    total: checks.length,
    passed: counts.passed,
    failed: counts.failed,
    running: counts.running,
    pending: counts.pending,
    completed,
    successRate: completed === 0 ? null : counts.passed / completed,
  };
}

/** A check run may carry an explicit run id on the wire (future-proof); when it
 * does and the mirror url is absent we reconstruct `actions/runs/{id}`. In
 * practice GitHub always normalizes the url, so that is the deep link used. */
type CheckWithRunId = GithubCheck & { id?: string | number; runId?: string | number };

/** Deep link for a check run's title: the mirror's own GitHub url, else the
 * canonical actions-runs path, else the repo's Actions tab — never a dead '#'. */
export function checkRunHref(c: CheckWithRunId): string {
  if (c.url) return c.url;
  const id = c.runId ?? c.id;
  if (id !== undefined && id !== null && `${id}` !== "") return githubRunHref(id);
  return `${GITHUB_REPO_URL}/actions`;
}

/** Human-readable byte size for an artifact (mirror carries sizeBytes). */
export function formatBytes(bytes: number | undefined): string | null {
  if (bytes === undefined || bytes === null || Number.isNaN(bytes)) return null;
  if (bytes < 1024) return `${bytes} B`;
  const units = ["KB", "MB", "GB", "TB"];
  let value = bytes / 1024;
  let i = 0;
  while (value >= 1024 && i < units.length - 1) {
    value /= 1024;
    i += 1;
  }
  return `${value < 10 ? value.toFixed(1) : Math.round(value)} ${units[i]}`;
}

/** Teal external-link glyph on every actionable title (mockup v2). */
function ExtGlyph() {
  return (
    <span className="gh-ext" aria-hidden="true">
      {" "}
      ↗
    </span>
  );
}

export function CiCdPipelineStatus({
  data,
  ghost = false,
}: {
  data: GithubStatus;
  /** Stale/errored mirror: keep last-good data visible but ghosted (degrade,
   * don't blank — DESIGN-SPEC §3). */
  ghost?: boolean;
}) {
  const checks = data.checkRuns;
  const artifacts = data.artifacts;

  // Nothing CI/CD-shaped in the mirror: render nothing (the tab's own empty
  // state owns that case), matching the panel contract this screen replaces.
  if (checks.length === 0 && artifacts.length === 0) return null;

  const summary = summarize(checks);
  const age = data.sync?.ageSeconds;
  const freshness = age === undefined ? "not synced yet" : `synced ${ageLabel(age)}`;

  return (
    <div
      className={`gh-screen gh-cicd${ghost ? " gh-screen--ghost" : ""}`}
      data-testid="gh-cicd"
      data-ghost={ghost ? "true" : "false"}
      aria-hidden={ghost ? "true" : undefined}
    >
      <header className="gh-screen__head">
        <div className="gh-screen__heading">
          <h2 className="gh-screen__title">CI/CD Pipeline Status</h2>
          <p className="gh-screen__sub">
            <span className="gh-screen__repo">{GITHUB_REPO_LABEL}</span>
            {` · ${checks.length} check run${checks.length === 1 ? "" : "s"} · `}
            {`${artifacts.length} artifact${artifacts.length === 1 ? "" : "s"} · `}
            <span data-testid="gh-cicd-freshness">{freshness}</span>
          </p>
        </div>
        <a
          className="gh-btn"
          href={`${GITHUB_REPO_URL}/actions`}
          target="_blank"
          rel="noreferrer noopener"
          data-testid="gh-cicd-open-github"
        >
          Open Actions on GitHub <ExtGlyph />
        </a>
      </header>

      <div className="gh-stats" data-testid="gh-cicd-summary">
        <Stat value={summary.passed} label="Passed" sub="completed green" tone="green" testId="gh-cicd-stat-passed" />
        <Stat value={summary.running} label="Running" sub="in progress" tone="blue" testId="gh-cicd-stat-running" />
        <Stat value={summary.failed} label="Failed" sub="needs attention" tone="red" testId="gh-cicd-stat-failed" />
        <Stat value={summary.pending} label="Pending" sub="queued" tone="amber" testId="gh-cicd-stat-pending" />
        <Stat
          value={summary.successRate === null ? "—" : `${Math.round(summary.successRate * 100)}%`}
          label="Success rate"
          sub={summary.completed === 0 ? "no completed runs yet" : `${summary.passed}/${summary.completed} completed`}
          tone="muted"
          testId="gh-cicd-stat-success-rate"
        />
      </div>

      <section className="gh-panel" data-testid="gh-cicd-visualization">
        <header className="gh-panel__head">
          <h3 className="gh-panel__title">Pipeline Stages</h3>
          <span className="gh-caption" style={{ margin: 0 }}>
            {checks.length === 0 ? "No check runs mirrored" : `${checks.length} stage${checks.length === 1 ? "" : "s"}`}
          </span>
        </header>
        <div className="gh-panel__body">
          {checks.length === 0 ? (
            <p className="gh-panel__empty" role="status" data-testid="gh-cicd-stages-empty">
              No pipeline activity mirrored yet.
            </p>
          ) : (
            <ol className="gh-cicd-stages" data-testid="gh-cicd-stages">
              {checks.map((c, i) => (
                <StageNode key={`stage-${c.name}-${i}`} check={c} last={i === checks.length - 1} />
              ))}
            </ol>
          )}
        </div>
      </section>

      {checks.length > 0 && (
        <section className="gh-panel" data-testid="panel-checks">
          <header className="gh-panel__head">
            <h3 className="gh-panel__title">Recent Check Runs</h3>
            <a
              className="gh-panel__action"
              href={`${GITHUB_REPO_URL}/actions`}
              target="_blank"
              rel="noreferrer noopener"
            >
              View all →
            </a>
          </header>
          <div className="gh-panel__body">
            <ul className="gh-cicd-runs" data-testid="gh-cicd-runs">
              {checks.map((c, i) => (
                <CheckRunRow key={`check-${c.name}-${i}`} check={c} />
              ))}
            </ul>
          </div>
        </section>
      )}

      {artifacts.length > 0 && (
        <section className="gh-panel" data-testid="panel-artifacts">
          <header className="gh-panel__head">
            <h3 className="gh-panel__title">Artifacts</h3>
          </header>
          <div className="gh-panel__body">
            <ul className="gh-cicd-artifacts" data-testid="gh-cicd-artifacts">
              {artifacts.map((a, i) => (
                <ArtifactRow key={`artifact-${a.name}-${i}`} artifact={a} />
              ))}
            </ul>
          </div>
        </section>
      )}
    </div>
  );
}

function StageNode({ check, last }: { check: GithubCheck; last: boolean }) {
  const status = checkStatus(check);
  return (
    <li
      className={`gh-cicd-stage gh-cicd-stage--${status}`}
      data-testid="gh-cicd-stage"
      data-status={status}
    >
      <a
        className="gh-cicd-stage__name"
        href={checkRunHref(check)}
        target="_blank"
        rel="noreferrer noopener"
      >
        {check.name}
        <ExtGlyph />
      </a>
      <span className="gh-cicd-stage__status" data-testid="gh-cicd-stage-status">
        {STATUS_LABEL[status]}
      </span>
      {!last && <span className="gh-cicd-stage__connector" aria-hidden="true" />}
    </li>
  );
}

function CheckRunRow({ check }: { check: GithubCheck }) {
  const status = checkStatus(check);
  const href = checkRunHref(check);
  const raw = check.conclusion || check.state;
  return (
    <li className="gh-cicd-run" data-testid="check-row" data-status={status}>
      <span className={`gh-cicd-dot gh-cicd-dot--${status}`} aria-hidden="true" />
      <a
        className="gh-cicd-run__name"
        href={href}
        target="_blank"
        rel="noreferrer noopener"
        data-testid="gh-cicd-run-link"
      >
        {check.name}
        <ExtGlyph />
      </a>
      {/* The raw provider verdict stays visible so the state is never obscured
          by the normalized label. */}
      <span className="gh-cicd-run__verdict muted">{raw}</span>
      <span className={`gh-badge gh-cicd-badge--${status}`} data-testid="gh-cicd-run-status">
        {STATUS_LABEL[status]}
      </span>
    </li>
  );
}

/** Humanize a mirror timestamp as "2 min ago"; null when absent/unparseable
 * (never an invented time). */
function timeAgo(iso: string | undefined): string | null {
  if (!iso) return null;
  const then = Date.parse(iso);
  if (Number.isNaN(then)) return null;
  return ageLabel(Math.max(0, (Date.now() - then) / 1000));
}

function ArtifactRow({ artifact }: { artifact: GithubArtifact }) {
  const size = formatBytes(artifact.sizeBytes);
  const created = timeAgo(artifact.createdAt);
  return (
    <li className="gh-cicd-artifact" data-testid="artifact-row">
      {artifact.url ? (
        <a
          className="gh-cicd-artifact__name"
          href={artifact.url}
          target="_blank"
          rel="noreferrer noopener"
          data-testid="gh-cicd-artifact-link"
        >
          {artifact.name}
          <ExtGlyph />
        </a>
      ) : (
        <span className="gh-cicd-artifact__name">{artifact.name}</span>
      )}
      {size && <span className="gh-cicd-artifact__size muted">{size}</span>}
      {created && <span className="gh-cicd-artifact__meta muted">{created}</span>}
    </li>
  );
}

function Stat({
  value,
  label,
  sub,
  tone,
  testId,
}: {
  value: number | string;
  label: string;
  sub: string;
  tone: "green" | "amber" | "blue" | "red" | "muted";
  testId: string;
}) {
  return (
    <div className={`gh-stat gh-stat--${tone}`} data-testid={testId}>
      <span className="gh-stat__value">{value}</span>
      <span className="gh-stat__label">{label}</span>
      <span className="gh-stat__label">{sub}</span>
    </div>
  );
}
