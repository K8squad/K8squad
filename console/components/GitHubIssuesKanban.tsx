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

import type { GithubIssue } from "@/lib/github-status";
import { ageLabel } from "@/lib/github-status";

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

export function GitHubIssuesKanban({ issues }: { issues: GithubIssue[] }) {
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
                    <IssueCard key={`issue-${issue.number}`} issue={issue} now={now} />
                  ))
                )}
              </div>
            </section>
          );
        })}
      </div>
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

function IssueCard({ issue, now }: { issue: GithubIssue; now: number }) {
  const priority = priorityFor(issue);
  const updated = updatedLabel(issue, now);
  const chips = labelsOf(issue).filter((l) => !isPriorityLabel(l));
  return (
    <article
      className={`gh-kanban-card${isBlocked(issue) ? " gh-kanban-card--blocked" : ""}`}
      data-testid="gh-issue-card"
      data-issue-number={issue.number}
    >
      <div className="gh-kanban-card__title">
        <IssueLink issue={issue} />
      </div>
      <div className="gh-kanban-card__tags">
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
