// components/github/PullRequestManagement.tsx — ISI-4672: the "Pull Request
// Management" GitHub screen (child of ISI-4662, "Github screen").
//
// Renders the scm-mirror projection's pull requests to match the board-approved
// "Pull Request Management" mockup: a PR workflow overview, an action row, a
// grid of detailed PR cards, and a selected-PR detail panel — in the
// IsItObservable dark palette.
//
// HONESTY (ADR-0013 §D4, carried from GitHubStatusTab): the mirror carries ONLY
// a PR's number, title, raw state (open/closed), mini-board reviewState
// (ready-for-review | merged), merged flag, branch, actor and updatedAt, plus a
// repo-level list of check runs. It does NOT carry per-PR reviewers, review
// comments, additions/deletions, related work items, agent tags or a GitHub
// merge-queue position. This screen therefore invents none of them:
//   - the workflow is the four stages derivable from state/merged/reviewState;
//   - "checks" are the repository's mirrored check runs, labelled as repo-level
//     (the mirror does not associate a run with an individual PR);
//   - recency is the mirror's honest "synced X ago" freshness and each PR's own
//     mirrored updatedAt ("last activity X ago"), never an invented timeline.
// Every actionable entity deep-links to GitHub via `pull/{n}` (AC2).

"use client";

import { useState } from "react";
import { ageLabel, type GithubPR, type GithubStatus } from "@/lib/github-status";
import { GITHUB_REPO_LABEL, GITHUB_REPO_URL, githubPullHref } from "@/lib/github-links";
import { ReviewAutomationDialog } from "./ReviewAutomationDialog";
import "./github-screens.css";
import "./prs.css";

/** The four workflow stages derivable from the mirror — the merge pipeline the
 * mockup draws (open → in-review → merged/closed). */
export type PrStage = "open" | "review" | "merged" | "closed";

/** Map a mirrored PR onto a workflow stage. `merged`/`reviewState` are the
 * mini-board projection (githubstatus.go); `state` is the raw provider state. */
export function prStage(pr: GithubPR): PrStage {
  const state = (pr.state || "").toLowerCase();
  const review = (pr.reviewState || "").toLowerCase();
  if (pr.merged || review === "merged" || state === "merged") return "merged";
  if (state === "closed") return "closed";
  if (review.includes("review")) return "review";
  return "open";
}

const STAGE_LABEL: Record<PrStage, string> = {
  open: "Open",
  review: "In review",
  merged: "Merged",
  closed: "Closed",
};

const STAGE_TONE: Record<PrStage, string> = {
  open: "blue",
  review: "amber",
  merged: "green",
  closed: "red",
};

/** The stages in merge-pipeline order, with honest counts (0 is real, not
 * fabricated — an empty stage is a real "no PRs here"). */
export function stageCounts(prs: GithubPR[]): Array<{ stage: PrStage; count: number }> {
  const order: PrStage[] = ["open", "review", "merged", "closed"];
  const counts: Record<PrStage, number> = { open: 0, review: 0, merged: 0, closed: 0 };
  for (const pr of prs) counts[prStage(pr)] += 1;
  return order.map((stage) => ({ stage, count: counts[stage] }));
}

/** A check-run verdict, mirroring the CI/CD screen's closed set. */
type CheckTone = "passed" | "failed" | "running" | "pending";

const PASS_CONCLUSIONS = new Set(["success", "neutral", "skipped"]);

export function checkTone(state: string, conclusion: string | undefined): CheckTone {
  const c = (conclusion || "").toLowerCase();
  if (c) return PASS_CONCLUSIONS.has(c) ? "passed" : "failed";
  const s = (state || "").toLowerCase();
  if (s === "in_progress" || s === "running") return "running";
  return "pending";
}

/** "2 days ago" from a mirror ISO timestamp; null when absent/unparseable —
 * never an invented time. */
function timeAgo(iso: string | undefined): string | null {
  if (!iso) return null;
  const then = Date.parse(iso);
  if (Number.isNaN(then)) return null;
  return ageLabel(Math.max(0, (Date.now() - then) / 1000));
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

export function PullRequestManagement({
  data,
  projectId,
  ghost = false,
}: {
  data: GithubStatus;
  /** The project whose review-automation config the settings dialog reads/writes
   * (ISI-4764). Absent (e.g. a stale caller) hides the settings button rather
   * than opening a dialog with no target. */
  projectId?: string;
  /** Stale/errored mirror: keep last-good data visible but ghosted (degrade,
   * don't blank — DESIGN-SPEC §3). */
  ghost?: boolean;
}) {
  const prs = data.pullRequests;
  const [selected, setSelected] = useState<number | null>(null);
  // ISI-4764: the "Review automation" settings dialog on the PR header.
  const [reviewAutomationOpen, setReviewAutomationOpen] = useState(false);

  // The tab's own empty state owns the no-data case; this screen renders
  // nothing so it composes cleanly with the other GitHub screens.
  if (prs.length === 0) return null;

  const stages = stageCounts(prs);
  const age = data.sync?.ageSeconds;
  const freshness = age === undefined ? "not synced yet" : `synced ${ageLabel(age)}`;

  const selectedPr = prs.find((p) => p.number === selected) ?? prs[0];

  return (
    <div
      className={`gh-screen gh-prs${ghost ? " gh-screen--ghost" : ""}`}
      data-testid="gh-prs"
      data-ghost={ghost ? "true" : "false"}
      aria-hidden={ghost ? "true" : undefined}
    >
      <header className="gh-screen__head">
        <div className="gh-screen__heading">
          <h2 className="gh-screen__title">Pull Request Management</h2>
          <p className="gh-screen__sub">
            <span className="gh-screen__repo">{GITHUB_REPO_LABEL}</span>
            {` · ${prs.length} pull request${prs.length === 1 ? "" : "s"} · `}
            <span data-testid="gh-prs-freshness">{freshness}</span>
          </p>
        </div>
        <div className="gh-screen__head-actions">
          {/* ISI-4764: unlike the read-only deep-links, this is a real in-console
              write path — it opens the E1-backed review-automation config. */}
          {projectId && (
            <button
              type="button"
              className="gh-btn"
              data-testid="gh-review-automation-btn"
              onClick={() => setReviewAutomationOpen(true)}
              aria-haspopup="dialog"
            >
              ⚙ Review automation
            </button>
          )}
          <a
            className="gh-btn"
            href={GITHUB_REPO_URL}
            target="_blank"
            rel="noreferrer noopener"
            data-testid="gh-prs-open-github"
          >
            Open on GitHub <ExtGlyph />
          </a>
        </div>
      </header>

      {reviewAutomationOpen && projectId && (
        <ReviewAutomationDialog
          projectId={projectId}
          onClose={() => setReviewAutomationOpen(false)}
        />
      )}

      {/* Action row: every control is a real GitHub action deep-link, never a
          fabricated in-console write path (the console is read-only over the
          mirror). */}
      <div className="gh-actions" data-testid="gh-pr-actions">
        <a
          className="gh-btn gh-btn--blue"
          href={`${GITHUB_REPO_URL}/pulls/new`}
          target="_blank"
          rel="noreferrer noopener"
        >
          New Pull Request <ExtGlyph />
        </a>
        <a
          className="gh-btn"
          href={`${GITHUB_REPO_URL}/pulls?q=is%3Aopen+is%3Adraft`}
          target="_blank"
          rel="noreferrer noopener"
        >
          Drafts <ExtGlyph />
        </a>
        <a
          className="gh-btn"
          href={`${GITHUB_REPO_URL}/pulls?q=is%3Aopen+review%3Arequired`}
          target="_blank"
          rel="noreferrer noopener"
        >
          Review Requests <ExtGlyph />
        </a>
        <a
          className="gh-btn"
          href={`${GITHUB_REPO_URL}/pulls?q=is%3Aopen`}
          target="_blank"
          rel="noreferrer noopener"
        >
          Filters <ExtGlyph />
        </a>
      </div>

      {/* PR workflow overview (mockup "PR Status Overview"): the merge pipeline
          derived from the mirror, with counts. */}
      <section className="gh-panel" data-testid="gh-pr-workflow">
        <header className="gh-panel__head">
          <h3 className="gh-panel__title">PR Status Overview</h3>
          <a
            className="gh-panel__action"
            href={`${GITHUB_REPO_URL}/pulls`}
            target="_blank"
            rel="noreferrer noopener"
          >
            View all on GitHub →
          </a>
        </header>
        <div className="gh-panel__body">
          <ol className="gh-pr-flow" data-testid="gh-pr-flow">
            {stages.map(({ stage, count }, i) => (
              <li
                className={`gh-pr-flow__node gh-pr-flow__node--${STAGE_TONE[stage]}`}
                data-testid="gh-pr-stage"
                data-stage={stage}
                key={stage}
              >
                <span className="gh-pr-flow__count" data-testid="gh-pr-stage-count">
                  {count}
                </span>
                <span className="gh-pr-flow__label">{STAGE_LABEL[stage]}</span>
                {i < stages.length - 1 && (
                  <span className="gh-pr-flow__connector" aria-hidden="true" />
                )}
              </li>
            ))}
          </ol>
          <p className="gh-caption" data-testid="gh-pr-workflow-note">
            Stages are derived from each mirrored pull request&apos;s raw state and review state. The
            mirror does not carry GitHub&apos;s merge-queue position, per-PR approvals or review
            comments, so they are not shown.
          </p>
        </div>
      </section>

      {/* PR cards grid (mockup "PR Cards"): branch, repo checks, review state
          and the mirrored actor/recency — no invented reviewers/agents. */}
      <div className="gh-pr-grid" data-testid="panel-prs">
        {prs.map((pr) => (
          <PrCard
            key={`pr-${pr.number}`}
            pr={pr}
            checks={data.checkRuns}
            selected={selectedPr.number === pr.number}
            onSelect={() => setSelected(pr.number)}
          />
        ))}
      </div>

      <PrDetail pr={selectedPr} checks={data.checkRuns} />
    </div>
  );
}

function PrCard({
  pr,
  checks,
  selected,
  onSelect,
}: {
  pr: GithubPR;
  checks: GithubStatus["checkRuns"];
  selected: boolean;
  onSelect: () => void;
}) {
  const stage = prStage(pr);
  const href = githubPullHref(pr.number, pr.url);
  const activity = timeAgo(pr.updatedAt);
  const rawReview = pr.reviewState || pr.state;

  return (
    <article
      className={`gh-pr-card${selected ? " gh-pr-card--selected" : ""}`}
      data-testid="pr-row"
      data-stage={stage}
    >
      <header className="gh-pr-card__head">
        {/* AC2: the PR title deep-links to GitHub via `pull/{n}`. */}
        <a
          className="gh-pr-card__title"
          href={href}
          target="_blank"
          rel="noreferrer noopener"
          data-testid="gh-pr-link"
        >
          #{pr.number}: {pr.title}
          <ExtGlyph />
        </a>
        <span
          className={`gh-badge gh-badge--${stage}`}
          data-testid="gh-pr-state"
        >
          {STAGE_LABEL[stage]}
        </span>
      </header>

      <dl className="gh-pr-card__meta">
        {/* AC1: branch. The mirror omits it for some rows — say so honestly. */}
        <div className="gh-pr-card__field">
          <dt className="muted">Branch</dt>
          <dd>
            {pr.branch ? (
              <code className="gh-pr-card__branch" data-testid="gh-pr-branch">
                {pr.branch}
              </code>
            ) : (
              <span className="muted" data-testid="gh-pr-branch">
                not mirrored
              </span>
            )}
          </dd>
        </div>
        {/* AC1: review state — raw mirror value, verbatim. */}
        <div className="gh-pr-card__field">
          <dt className="muted">Review</dt>
          <dd>
            <span data-testid="gh-pr-review-state">{rawReview}</span>
          </dd>
        </div>
      </dl>

      {/* AC1: checks. Repo-level, because the mirror has no per-PR association. */}
      <div className="gh-pr-card__checks" data-testid="gh-pr-checks">
        <span className="gh-pr-card__checks-label muted">Repo checks</span>
        {checks.length === 0 ? (
          <span className="muted">none mirrored</span>
        ) : (
          <ul className="gh-pr-card__check-list">
            {checks.map((c, i) => {
              const tone = checkTone(c.state, c.conclusion);
              return (
                <li
                  className="gh-pr-card__check"
                  data-testid="gh-pr-check"
                  data-status={tone}
                  key={`check-${c.name}-${i}`}
                >
                  <span className={`gh-pr-dot gh-pr-dot--${tone}`} aria-hidden="true" />
                  {c.name}
                </li>
              );
            })}
          </ul>
        )}
      </div>

      <footer className="gh-pr-card__foot">
        <span className="muted">
          {pr.actor ? `@${pr.actor} · ` : ""}
          {activity ? `last activity ${activity}` : "last activity not mirrored"}
        </span>
        <button
          type="button"
          className="gh-pr-card__detail"
          onClick={onSelect}
          data-testid="gh-pr-detail-btn"
          aria-pressed={selected}
        >
          {selected ? "Showing" : "Details"}
        </button>
      </footer>
    </article>
  );
}

/** The selected-PR detail panel (mockup "PR #142: Detailed View"). Renders only
 * the fields the mirror actually holds plus honest GitHub action deep-links. */
function PrDetail({ pr, checks }: { pr: GithubPR; checks: GithubStatus["checkRuns"] }) {
  const href = githubPullHref(pr.number, pr.url);
  const activity = timeAgo(pr.updatedAt);
  const stage = prStage(pr);

  return (
    <section className="gh-panel" data-testid="gh-pr-detail">
      <header className="gh-panel__head">
        <h3 className="gh-panel__title">
          PR #{pr.number}: Detailed View
        </h3>
        <a
          className="gh-panel__action"
          href={href}
          target="_blank"
          rel="noreferrer noopener"
          data-testid="gh-pr-detail-link"
        >
          Open on GitHub ↗
        </a>
      </header>
      <div className="gh-panel__body">
        <dl className="gh-pr-detail__fields">
          <div>
            <dt className="muted">Title</dt>
            <dd>{pr.title}</dd>
          </div>
          <div>
            <dt className="muted">State</dt>
            <dd data-testid="gh-pr-detail-state">{STAGE_LABEL[stage]}</dd>
          </div>
          <div>
            <dt className="muted">Branch</dt>
            <dd>{pr.branch || "not mirrored"}</dd>
          </div>
          <div>
            <dt className="muted">Author</dt>
            <dd>{pr.actor ? `@${pr.actor}` : "not mirrored"}</dd>
          </div>
          <div>
            <dt className="muted">Last activity</dt>
            <dd>{activity ?? "not mirrored"}</dd>
          </div>
          <div>
            <dt className="muted">Check runs</dt>
            <dd>{checks.length}</dd>
          </div>
        </dl>
        {/* Merge / review / close happen on GitHub; the console is read-only
            over the mirror, so these are deep-links, not fake write buttons. */}
        <div className="gh-pr-detail__actions">
          <a
            className="gh-btn gh-btn--blue"
            href={href}
            target="_blank"
            rel="noreferrer noopener"
          >
            Open Pull Request <ExtGlyph />
          </a>
          <a
            className="gh-btn"
            href={`${href}/files`}
            target="_blank"
            rel="noreferrer noopener"
          >
            View Changes <ExtGlyph />
          </a>
          <a
            className="gh-btn"
            href={`${GITHUB_REPO_URL}/actions`}
            target="_blank"
            rel="noreferrer noopener"
          >
            Checks <ExtGlyph />
          </a>
        </div>
      </div>
    </section>
  );
}
