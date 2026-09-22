"use client";

// components/GitHubIssuesKanban.tsx — ISI-4673: the Issues Kanban board for the
// GitHub-status tab, matching the approved mockup (ISI-4662 §3 "Issues Kanban
// Board").
//
// HONEST by construction (ADR-0013 §D4): every card is a real mirrored issue.
// Columns are a PURE PROJECTION of the issue's provider state + its own labels —
// never a fabricated lane or a made-up row:
//   closed                                     → Done
//   open + an "in progress"/"wip" label         → In Progress
//   open + a "todo"/"ready"/"next" label        → To Do
//   open (no such label)                        → Backlog
// The priority badge is derived from the provider's REAL label set (priority:
// high, P1, critical, …); an issue with no priority label shows NO badge rather
// than an invented one, and the priority queue only ever holds labelled issues.
// Assignees are the mirror's own assignees; absent renders "Unassigned".
//
// Every title deep-links to GitHub (normalized mirror url, else the repo base +
// /issues/{n}) and carries the teal ↗ glyph (ISI-4662 v2 board feedback).
//
// ISI-4758 (ISI-4749 epic 1): a Kanban card is now a click target that opens a
// per-issue popup (GitHubIssuePopup). The popup is a FE SHELL only — it shows the
// issue identity, hosts the "Open on GitHub ↗" deep-link (moved off the card so
// the card click no longer navigates away), and reserves an empty assign slot
// (data-testid="gh-issue-assign-slot") that ISI-4749 epic 3 fills with the
// agent-assign / dispatch control. No dispatch is wired here. The priority queue
// keeps its direct GitHub deep-links (it is a list, not a card).

import { useEffect, useId, useRef, useState } from "react";
import type { GithubIssue } from "@/lib/github-status";
import { ageLabel } from "@/lib/github-status";
import {
  assignAndDispatch,
  listSquadAgents,
  type AgentOption,
  type GithubAssignErrorCode,
  type GithubAssignResult,
} from "@/lib/tickets/api";

/** Fallback repo base when a mirror row carries no normalized url. The issue's
 * own url is always preferred; this only keeps the deep-link affordance on a
 * sparse row (matches the approved mockup's repo header). */
const FALLBACK_REPO_BASE = "https://github.com/K8squad/K8squad";
const FALLBACK_REPO_SLUG = "K8squad/K8squad";

type ColumnKey = "backlog" | "todo" | "in-progress" | "done";

const COLUMNS: ReadonlyArray<{ key: ColumnKey; label: string; hint: string }> = [
  { key: "backlog", label: "Backlog", hint: "Open, not yet scheduled" },
  { key: "todo", label: "To Do", hint: "Scheduled, ready to pick up" },
  { key: "in-progress", label: "In Progress", hint: "Work underway" },
  { key: "done", label: "Done", hint: "Closed issues" },
];

const IN_PROGRESS_RE = /(^|[^a-z])(in[\s-]?progress|in[\s-]?review|wip|doing)([^a-z]|$)/i;
const TODO_RE = /(^|[^a-z])(to[\s-]?do|todo|ready|next|planned|triage)([^a-z]|$)/i;
const BLOCKED_RE = /(^|[^a-z])(blocked|blocker|on[\s-]?hold|waiting)([^a-z]|$)/i;

type Priority = { rank: number; label: string; tone: "critical" | "high" | "medium" | "low" };

// Priority rules read ONLY the provider's label text; no labels ⇒ no priority.
const PRIORITY_RULES: ReadonlyArray<{ re: RegExp; rank: number; label: string; tone: Priority["tone"] }> = [
  { re: /(^|[^a-z])(p0|sev0|sev1|critical|urgent)([^a-z]|$)/i, rank: 0, label: "Critical", tone: "critical" },
  { re: /(^|[^a-z])(p1|high)([^a-z]|$)/i, rank: 1, label: "High", tone: "high" },
  { re: /(^|[^a-z])(p2|medium|normal)([^a-z]|$)/i, rank: 2, label: "Medium", tone: "medium" },
  { re: /(^|[^a-z])(p3|p4|low|minor)([^a-z]|$)/i, rank: 3, label: "Low", tone: "low" },
];

function labelsOf(issue: GithubIssue): string[] {
  return issue.labels ?? [];
}

function isClosed(issue: GithubIssue): boolean {
  return (issue.state ?? "").toLowerCase() === "closed";
}

function isBlocked(issue: GithubIssue): boolean {
  return labelsOf(issue).some((l) => BLOCKED_RE.test(l));
}

/** columnFor is the pure projection of provider state + labels onto a column. */
export function columnFor(issue: GithubIssue): ColumnKey {
  if (isClosed(issue)) return "done";
  const labels = labelsOf(issue);
  if (labels.some((l) => IN_PROGRESS_RE.test(l))) return "in-progress";
  if (labels.some((l) => TODO_RE.test(l))) return "todo";
  return "backlog";
}

/** priorityFor returns the highest-ranked priority a label names, or null — the
 * honest absence (no badge) when the provider carried no priority label. */
export function priorityFor(issue: GithubIssue): Priority | null {
  let best: Priority | null = null;
  for (const label of labelsOf(issue)) {
    for (const rule of PRIORITY_RULES) {
      if (rule.re.test(label) && (best === null || rule.rank < best.rank)) {
        best = { rank: rule.rank, label: rule.label, tone: rule.tone };
      }
    }
  }
  return best;
}

function isPriorityLabel(label: string): boolean {
  return PRIORITY_RULES.some((r) => r.re.test(label));
}

/** LocalRunBadge is the clearly-LOCAL status chip (ISI-4760, Epic 4). Its tone
 * mirrors the work-item run state from the Epic 2 read contract (ISI-4757 §7). */
export type LocalRunBadge = { label: string; tone: "running" | "queued" | "done" };

/** localRunBadge derives the honest local overlay from `issue.local` — the
 * OPTIONAL bridge block the apiserver attaches only for a GitHub issue linked to
 * a Paperclip work-item (contract §7). It is a PURE projection:
 *   • absent `local`                       → null (AC4: non-bridged card unchanged)
 *   • runState "running"                    → "<agent> working…"  (AC1)
 *   • runState "todo"                       → "<agent> queued"
 *   • runState "done"                       → "<agent> finished"  (AC3 terminal)
 * It reads ONLY `local` and NEVER `assignees` — the badge is a local status, not
 * a GitHub assignee (ADR-0013 / contract §6 honesty guard). The BE never sends
 * enum values outside the §7 set, so any unrecognised state yields null rather
 * than an invented chip. */
export function localRunBadge(issue: GithubIssue): LocalRunBadge | null {
  const local = issue.local;
  if (!local) return null;
  const agent = local.agent?.trim();
  if (!agent) return null;
  switch (local.runState) {
    case "running":
      return { label: `${agent} working…`, tone: "running" };
    case "todo":
      return { label: `${agent} queued`, tone: "queued" };
    case "done":
      return { label: `${agent} finished`, tone: "done" };
    default:
      return null;
  }
}

/** issueHref prefers the mirror's normalized url; a sparse row still gets the
 * repo-base deep link so the AC "titles deep-link via issues/{n}" always holds. */
export function issueHref(issue: GithubIssue): string | undefined {
  if (issue.url) return issue.url;
  if (issue.number > 0) return `${FALLBACK_REPO_BASE}/issues/${issue.number}`;
  return undefined;
}

function repoSlug(issues: GithubIssue[]): string {
  for (const issue of issues) {
    const m = issue.url?.match(/github\.com\/([^/]+)\/([^/]+)/i);
    if (m) return `${m[1]}/${m[2]}`;
  }
  return FALLBACK_REPO_SLUG;
}

function repoBase(issues: GithubIssue[]): string {
  for (const issue of issues) {
    const m = issue.url?.match(/^(https?:\/\/github\.com\/[^/]+\/[^/]+)/i);
    if (m) return m[1];
  }
  return FALLBACK_REPO_BASE;
}

function updatedLabel(issue: GithubIssue, now: number): string | null {
  if (!issue.updatedAt) return null;
  const then = Date.parse(issue.updatedAt);
  if (Number.isNaN(then)) return null;
  return `updated ${ageLabel((now - then) / 1000)}`;
}

export function GitHubIssuesKanban({
  issues,
  projectId,
  onAssigned,
}: {
  issues: GithubIssue[];
  /** Project context for the Epic-3 assign-&-dispatch action. Absent (e.g. in the
   * shell unit tests) ⇒ the popup renders without the assign control — honest: the
   * bridge is project-scoped and cannot dispatch without it. */
  projectId?: string;
  /** Handed the whole Epic-2 bridge result on a successful dispatch so a parent
   * (Epic 4, ISI-4760) can reflect the local assignment. Optional so the Epic-1
   * shell compiles before Epic 4 lands. */
  onAssigned?: (result: GithubAssignResult) => void;
}) {
  // The card the operator clicked, if any — drives the per-issue popup (ISI-4758).
  const [selected, setSelected] = useState<GithubIssue | null>(null);

  if (issues.length === 0) return null;

  const now = Date.now();
  const grouped = new Map<ColumnKey, GithubIssue[]>(COLUMNS.map((c) => [c.key, []]));
  for (const issue of issues) grouped.get(columnFor(issue))!.push(issue);

  const open = issues.filter((i) => !isClosed(i)).length;
  const blocked = issues.filter(isBlocked).length;
  const inProgress = grouped.get("in-progress")?.length ?? 0;
  const prioritized = issues
    .map((issue) => ({ issue, priority: priorityFor(issue) }))
    .filter((x): x is { issue: GithubIssue; priority: Priority } => x.priority !== null)
    .sort(
      (a, b) =>
        a.priority.rank - b.priority.rank ||
        (Date.parse(b.issue.updatedAt ?? "") || 0) - (Date.parse(a.issue.updatedAt ?? "") || 0),
    );
  const assignees = new Set(issues.flatMap((i) => i.assignees ?? []));

  return (
    <section className="gh-issues" data-testid="panel-issues">
      <header className="gh-issues__head">
        <h2>Issues</h2>
        <span className="muted" data-testid="gh-issues-summary">
          {repoSlug(issues)} · {open} open · {blocked} blocked
        </span>
        <a
          className="gh-issues__repo"
          href={repoBase(issues)}
          target="_blank"
          rel="noreferrer noopener"
          data-testid="gh-issues-open-github"
        >
          Open on GitHub <span aria-hidden="true">↗</span>
        </a>
      </header>

      <ul className="gh-issues__stats" data-testid="gh-issues-stats">
        <li>
          <span className="gh-issues__stat-value">{open}</span>
          <span className="muted">Open</span>
        </li>
        <li>
          <span className="gh-issues__stat-value">{inProgress}</span>
          <span className="muted">In progress</span>
        </li>
        <li>
          <span className="gh-issues__stat-value">{blocked}</span>
          <span className="muted">Blocked</span>
        </li>
        <li>
          <span className="gh-issues__stat-value">{prioritized.length}</span>
          <span className="muted">Prioritised</span>
        </li>
        <li>
          <span className="gh-issues__stat-value">{assignees.size}</span>
          <span className="muted">Assignees</span>
        </li>
      </ul>

      {prioritized.length > 0 && (
        <div className="gh-priority-queue" data-testid="gh-priority-queue">
          <h3 className="gh-priority-queue__title">Priority Queue</h3>
          <ul>
            {prioritized.slice(0, 5).map(({ issue, priority }) => (
              <li key={`pq-${issue.number}`} className={`gh-priority-item gh-priority-item--${priority.tone}`}>
                <IssueLink issue={issue} />
                <span className={`gh-priority-badge gh-priority-badge--${priority.tone}`}>
                  {priority.label}
                </span>
                <span className="muted">
                  {issue.assignees && issue.assignees.length > 0
                    ? `Assigned to ${issue.assignees.join(", ")}`
                    : "Unassigned"}
                </span>
              </li>
            ))}
          </ul>
        </div>
      )}

      <div className="gh-kanban" data-testid="gh-issues-kanban">
        {COLUMNS.map((col) => {
          const items = grouped.get(col.key) ?? [];
          return (
            <section
              key={col.key}
              className={`gh-kanban-col gh-kanban-col--${col.key}`}
              data-testid={`gh-kanban-col-${col.key}`}
              aria-label={col.label}
              title={col.hint}
            >
              <header className="gh-kanban-col__head">
                <span className="gh-kanban-col__name">{col.label}</span>
                <span className="gh-kanban-col__count" data-testid={`gh-kanban-count-${col.key}`}>
                  {items.length}
                </span>
              </header>
              <div className="gh-kanban-col__cards">
                {items.length === 0 ? (
                  <p className="gh-kanban-col__empty muted">None</p>
                ) : (
                  items.map((issue) => (
                    <IssueCard
                      key={`issue-${issue.number}`}
                      issue={issue}
                      now={now}
                      onOpen={setSelected}
                    />
                  ))
                )}
              </div>
            </section>
          );
        })}
      </div>

      {selected && (
        <GitHubIssuePopup
          issue={selected}
          projectId={projectId}
          onClose={() => setSelected(null)}
          onAssigned={onAssigned}
        />
      )}
    </section>
  );
}

function IssueLink({ issue }: { issue: GithubIssue }) {
  const href = issueHref(issue);
  const body = (
    <>
      <span className="gh-issue-num">#{issue.number}</span> {issue.title}
      <span className="gh-issue-ext" aria-hidden="true">
        ↗
      </span>
    </>
  );
  if (!href) return <span data-testid="gh-issue-title">{body}</span>;
  return (
    <a
      className="gh-issue-title"
      data-testid="gh-issue-title"
      href={href}
      target="_blank"
      rel="noreferrer noopener"
    >
      {body}
    </a>
  );
}

function IssueCard({
  issue,
  now,
  onOpen,
}: {
  issue: GithubIssue;
  now: number;
  onOpen: (issue: GithubIssue) => void;
}) {
  const priority = priorityFor(issue);
  const updated = updatedLabel(issue, now);
  const chips = labelsOf(issue).filter((l) => !isPriorityLabel(l));
  // The whole card is the click target now (ISI-4758): clicking or pressing
  // Enter/Space opens the popup. role="button" + tabIndex make it keyboard
  // focusable; the GitHub deep-link lives inside the popup, not on the card, so a
  // card click never navigates away.
  const open = () => onOpen(issue);
  // ISI-4760 honesty guard (ADR-0013 / contract §6): the local badge is derived
  // PURELY from `issue.local` and is NEVER merged into the mirrored `assignees`
  // line below. This render path issues no GitHub write of any kind.
  const localBadge = localRunBadge(issue);
  return (
    <article
      className={`gh-kanban-card gh-kanban-card--clickable${isBlocked(issue) ? " gh-kanban-card--blocked" : ""}`}
      data-testid="gh-issue-card"
      data-issue-number={issue.number}
      role="button"
      tabIndex={0}
      aria-haspopup="dialog"
      onClick={open}
      onKeyDown={(e) => {
        if (e.key === "Enter" || e.key === " ") {
          e.preventDefault();
          open();
        }
      }}
    >
      <div className="gh-kanban-card__title">
        <span className="gh-issue-title" data-testid="gh-issue-title">
          <span className="gh-issue-num">#{issue.number}</span> {issue.title}
        </span>
      </div>
      <div className="gh-kanban-card__tags">
        {localBadge && (
          <span
            className={`gh-local-badge gh-local-badge--${localBadge.tone}`}
            data-testid="gh-issue-local-run"
            aria-label={`Local Paperclip status: ${localBadge.label}`}
          >
            <span className="gh-local-badge__dot" aria-hidden="true" />
            {localBadge.label}
          </span>
        )}
        {priority && (
          <span
            className={`gh-priority-badge gh-priority-badge--${priority.tone}`}
            data-testid="gh-issue-priority"
          >
            {priority.label}
          </span>
        )}
        {isBlocked(issue) && (
          <span className="gh-priority-badge gh-priority-badge--blocked" data-testid="gh-issue-blocked">
            Blocked
          </span>
        )}
        {chips.slice(0, 4).map((label) => (
          <span className="gh-label-chip" key={label} data-testid="gh-issue-label">
            {label}
          </span>
        ))}
        {chips.length > 4 && <span className="gh-label-chip muted">+{chips.length - 4}</span>}
      </div>
      <div className="gh-kanban-card__meta">
        <span
          className={`gh-issue-assignee${issue.assignees?.length ? "" : " muted"}`}
          data-testid="gh-issue-assignee"
        >
          {issue.assignees?.length ? issue.assignees.join(", ") : "Unassigned"}
        </span>
        {updated && (
          <span className="muted gh-issue-updated" data-testid="gh-issue-updated">
            {updated}
          </span>
        )}
      </div>
    </article>
  );
}

/**
 * GitHubIssuePopup — ISI-4758 (ISI-4749 epic 1). The FE shell opened by clicking a
 * Kanban card. It reuses the tickets sheet's modal ergonomics (role="dialog",
 * aria-modal, scrim-click + Escape to close, focus moved inside on open and
 * restored to the trigger on close) but centres a compact popup rather than a
 * slide-over.
 *
 * Contents are intentionally minimal — this epic ships the shell only:
 *   • the issue identity (number + title) as the dialog's accessible name,
 *   • an EMPTY assign slot (data-testid="gh-issue-assign-slot") that ISI-4749
 *     epic 3 fills with the agent-assign / dispatch control, and
 *   • the "Open on GitHub ↗" deep-link (reusing issueHref) that opens the issue
 *     in a new tab — moved here off the card so a card click opens the popup
 *     instead of navigating away.
 * No dispatch is wired here.
 */
function GitHubIssuePopup({
  issue,
  projectId,
  onClose,
  onAssigned,
}: {
  issue: GithubIssue;
  projectId?: string;
  onClose: () => void;
  onAssigned?: (result: GithubAssignResult) => void;
}) {
  const href = issueHref(issue);
  const titleId = useId();
  const closeRef = useRef<HTMLButtonElement>(null);

  // Move focus into the dialog on open (a11y), close on Escape, and restore focus
  // to whatever opened it (the card) on unmount.
  useEffect(() => {
    const opener = document.activeElement as HTMLElement | null;
    closeRef.current?.focus();
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose();
    };
    window.addEventListener("keydown", onKey);
    return () => {
      window.removeEventListener("keydown", onKey);
      opener?.focus?.();
    };
  }, [onClose]);

  return (
    <div
      className="gh-issue-popup-scrim"
      data-testid="gh-issue-popup-scrim"
      onClick={onClose}
    >
      <div
        className="gh-issue-popup"
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        data-testid="gh-issue-popup"
        // Clicks inside the popup must not fall through to the scrim's close.
        onClick={(e) => e.stopPropagation()}
      >
        <header className="gh-issue-popup__head">
          <h2 id={titleId} className="gh-issue-popup__title">
            <span className="gh-issue-num">#{issue.number}</span> {issue.title}
          </h2>
          <button
            ref={closeRef}
            type="button"
            className="gh-issue-popup__close"
            aria-label="Close"
            data-testid="gh-issue-popup-close"
            onClick={onClose}
          >
            ×
          </button>
        </header>

        <div className="gh-issue-popup__body">
          {/* Epic 3 (ISI-4759) fills this slot with the agent-assign / dispatch
              control. Rendered only with a project context — the bridge is
              project-scoped, so without a projectId the slot stays empty rather
              than showing a non-functional (fabricated) affordance (ADR-0013). */}
          <div className="gh-issue-popup__assign" data-testid="gh-issue-assign-slot">
            {projectId && (
              <AssignAndDispatch
                projectId={projectId}
                issue={issue}
                onClose={onClose}
                onAssigned={onAssigned}
              />
            )}
          </div>

          {href && (
            <a
              className="gh-issue-popup__gh-link"
              href={href}
              target="_blank"
              rel="noreferrer noopener"
              data-testid="gh-issue-popup-open-github"
            >
              Open on GitHub <span aria-hidden="true">↗</span>
            </a>
          )}
        </div>
      </div>
    </div>
  );
}

/** Map an Epic-2 status/error code to the operator-facing message (story §6).
 * A 409 ("already assigned") is deliberately informational, not an alarm —
 * idempotent re-assign is safe (the bridge dedupes on the issue label). */
function assignErrorMessage(code: GithubAssignErrorCode): string {
  switch (code) {
    case "invalid-agent":
      return "Pick a valid agent to assign.";
    case "forbidden":
      return "You can't dispatch this agent to this issue.";
    case "already":
      return "This issue is already assigned to an agent.";
    case "not-found":
      return "Couldn't find this issue to assign.";
    case "not-hosted":
      return "Agent assignment isn't available in this environment.";
    case "failed":
    default:
      return "Assignment failed — try again.";
  }
}

type RosterState =
  | { kind: "loading" }
  | { kind: "ready"; agents: AgentOption[] }
  | { kind: "empty" };

/**
 * AssignAndDispatch — the Epic-3 control that fills the Epic-1 popup's assign slot
 * (ISI-4759 / ISI-4749 epic 3). On mount it loads the squad roster
 * (listSquadAgents, best-effort → []); the operator picks an agent by NAME and
 * clicks "Assign & dispatch", which calls the Epic-2 bridge via its BFF
 * (assignAndDispatch). Success hands the whole result up (onAssigned, for Epic 4)
 * and closes the popup; a failure surfaces inline (story §6) and preserves the
 * selection so the operator can retry. Human-only custody is honoured: the action
 * is a human console click carrying the session cookie, and no GitHub assignee is
 * ever written (ADR-0013).
 */
function AssignAndDispatch({
  projectId,
  issue,
  onClose,
  onAssigned,
}: {
  projectId: string;
  issue: GithubIssue;
  onClose: () => void;
  onAssigned?: (result: GithubAssignResult) => void;
}) {
  const [roster, setRoster] = useState<RosterState>({ kind: "loading" });
  const [agentName, setAgentName] = useState<string>("");
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const selectId = useId();

  useEffect(() => {
    let alive = true;
    void listSquadAgents().then((agents) => {
      if (!alive) return;
      setRoster(agents.length > 0 ? { kind: "ready", agents } : { kind: "empty" });
    });
    return () => {
      alive = false;
    };
  }, []);

  const rosterReady = roster.kind === "ready";
  const canSubmit = rosterReady && agentName !== "" && !submitting;

  async function onSubmit() {
    if (!canSubmit) return;
    setSubmitting(true);
    setError(null);
    const outcome = await assignAndDispatch(projectId, issue, agentName);
    if (outcome.ok) {
      onAssigned?.(outcome.result);
      onClose();
      return; // popup is unmounting — don't touch local state after close
    }
    setError(assignErrorMessage(outcome.code));
    setSubmitting(false); // keep selection; let the operator retry
  }

  return (
    <div className="gh-issue-assign" data-testid="gh-issue-assign">
      <label className="gh-issue-assign__label" htmlFor={selectId}>
        Assign to agent
      </label>
      <select
        id={selectId}
        className="gh-issue-assign__select"
        data-testid="gh-issue-assign-select"
        value={agentName}
        disabled={!rosterReady || submitting}
        onChange={(e) => {
          setAgentName(e.target.value);
          if (error) setError(null);
        }}
      >
        {roster.kind === "loading" && <option value="">Loading agents…</option>}
        {roster.kind === "empty" && <option value="">No agents available</option>}
        {roster.kind === "ready" && (
          <>
            <option value="">Select an agent…</option>
            {roster.agents.map((a) => (
              <option key={a.id} value={a.name}>
                {a.name}
              </option>
            ))}
          </>
        )}
      </select>

      <button
        type="button"
        className="gh-issue-assign__submit"
        data-testid="gh-issue-assign-submit"
        disabled={!canSubmit}
        onClick={() => void onSubmit()}
      >
        {submitting ? "Assigning…" : "Assign & dispatch"}
      </button>

      {error && (
        <p className="gh-issue-assign__error" role="alert" data-testid="gh-issue-assign-error">
          {error}
        </p>
      )}
    </div>
  );
}
