"use client";

// components/projects/ProjectLanding.tsx — S2 (ISI-3958) landing payload for the
// Project-Detail Workspace. Three read-only panels mounted inside the S1 Landing frame
// (ISI-3957): Runs · Stats · Latest Tickets. Story:
// ksquad/docs/bmad/stories/isi-3946-s2-landing-runs-stats-tickets.md
//
// Everything here is a REPROJECTION of reads the console already issues — no new
// back-end (AC5). Runs + the phase rollup come from GET /api/squad/overview (the same
// SquadOverview projection, sliced to ONE project by name); Latest Tickets + the
// open/closed count come from GET /api/projects/{id}/work-items via listWorkItems.
//
// Two fetches, three panels: overview feeds Runs + Stats(phase); work-items feeds
// Latest-Tickets + Stats(open/closed). Each fetch owns its own async boundary so a slow
// or 501 work-items read degrades ONLY its consumers — Runs/Stats(phase) stay populated
// (AC4 per-panel isolation). Fabrication discipline (FR-I3, AC2): a value whose source
// is UNAVAILABLE renders "—"; only an authoritative zero renders 0.

import { useEffect, useMemo, useState } from "react";
import {
  classifyOverviewStatus,
  phaseTone,
  type SquadOverviewData,
} from "@/components/SquadOverview";
import { ApiError, listWorkItems } from "@/lib/tickets/api";
import { STATE_LABELS, type WorkItem } from "@/lib/tickets/types";

/** One project entry from the team-scoped overview projection. */
export type OverviewProject = NonNullable<SquadOverviewData["projects"]>[number];
type OverviewRun = NonNullable<OverviewProject["runs"]>[number];

export type OverviewState =
  | { kind: "loading" }
  | { kind: "unauthenticated" }
  | { kind: "no-team" }
  | { kind: "not-wired" }
  | { kind: "error"; status: number }
  | { kind: "ready"; project: OverviewProject | null };

export type WorkItemsState =
  | { kind: "loading" }
  | { kind: "not-available" } // read model unhosted (501) or absent (404) — degrade honestly
  | { kind: "error"; status: number }
  | { kind: "ready"; items: WorkItem[] };

const DEFAULT_RUNS_LIMIT = 8;
const DEFAULT_TICKETS_LIMIT = 6;

// ---------------------------------------------------------------------------
// Pure projection helpers (exported for unit tests — no fetch, no DOM).
// ---------------------------------------------------------------------------

/** Slice the team overview down to the single project (matched by name, story decision (a)). */
export function selectProject(
  data: SquadOverviewData,
  projectId: string,
): OverviewProject | null {
  return (data.projects ?? []).find((p) => p.name === projectId) ?? null;
}

/** Most-recent runs first (claimedAt desc; unclaimed sort last), capped at `limit`. */
export function recentRuns(project: OverviewProject | null, limit: number): OverviewRun[] {
  const runs = project?.runs ?? [];
  return [...runs]
    .sort((a, b) => (Date.parse(b.claimedAt ?? "") || 0) - (Date.parse(a.claimedAt ?? "") || 0))
    .slice(0, limit);
}

/** Most-recently-updated work items first, capped at `limit`. */
export function recentTickets(items: WorkItem[], limit: number): WorkItem[] {
  return [...items]
    .sort((a, b) => (Date.parse(b.updatedAt) || 0) - (Date.parse(a.updatedAt) || 0))
    .slice(0, limit);
}

/** Open vs closed partition — closed ≡ state "done", everything else open (AC2). */
export function partitionIssues(items: WorkItem[]): { open: number; closed: number } {
  let closed = 0;
  for (const it of items) if (it.state === "done") closed += 1;
  return { open: items.length - closed, closed };
}

/** Newest activity timestamp across runs (claimedAt) + tickets (updatedAt); null ⇒ none known. */
export function lastActivity(
  project: OverviewProject | null,
  items: WorkItem[],
): string | null {
  let max = 0;
  for (const r of project?.runs ?? []) {
    const t = Date.parse(r.claimedAt ?? "");
    if (t) max = Math.max(max, t);
  }
  for (const it of items) {
    const t = Date.parse(it.updatedAt);
    if (t) max = Math.max(max, t);
  }
  return max ? new Date(max).toISOString() : null;
}

// ---------------------------------------------------------------------------
// Data hooks — each fetch is an independent async boundary (AC4).
// ---------------------------------------------------------------------------

function useOverviewProject(projectId: string): OverviewState {
  const [state, setState] = useState<OverviewState>({ kind: "loading" });
  useEffect(() => {
    let alive = true;
    setState({ kind: "loading" });
    fetch("/api/squad/overview", { headers: { accept: "application/json" } })
      .then(async (res) => {
        if (!alive) return;
        if (!res.ok) {
          const cls = classifyOverviewStatus(res.status);
          // classifyOverviewStatus never returns "ready" for a non-ok status.
          setState(cls as Exclude<OverviewState, { kind: "ready" }>);
          return;
        }
        const data = (await res.json()) as SquadOverviewData;
        setState({ kind: "ready", project: selectProject(data, projectId) });
      })
      .catch(() => {
        if (alive) setState({ kind: "error", status: 0 });
      });
    return () => {
      alive = false;
    };
  }, [projectId]);
  return state;
}

function useProjectWorkItems(projectId: string): WorkItemsState {
  const [state, setState] = useState<WorkItemsState>({ kind: "loading" });
  useEffect(() => {
    let alive = true;
    setState({ kind: "loading" });
    listWorkItems(projectId)
      .then((items) => {
        if (alive) setState({ kind: "ready", items });
      })
      .catch((err: unknown) => {
        if (!alive) return;
        const status = err instanceof ApiError ? err.status : 0;
        setState(
          status === 501 || status === 404
            ? { kind: "not-available" }
            : { kind: "error", status },
        );
      });
    return () => {
      alive = false;
    };
  }, [projectId]);
  return state;
}

// ---------------------------------------------------------------------------
// Presentational panels.
// ---------------------------------------------------------------------------

function OverviewProblem({ state }: { state: Exclude<OverviewState, { kind: "ready" | "loading" }> }) {
  switch (state.kind) {
    case "unauthenticated":
      return <p className="muted" style={{ margin: 0 }}>Sign in to view this project.</p>;
    case "no-team":
      return <p className="muted" style={{ margin: 0 }}>No projection for your Team yet.</p>;
    case "not-wired":
      return <p className="muted" style={{ margin: 0 }}>Overview read model not wired on this deployment.</p>;
    default:
      return (
        <p className="muted" style={{ margin: 0 }}>
          Overview unavailable (HTTP {state.status || "network error"}).
        </p>
      );
  }
}

export function RunsPanel({ state, limit = DEFAULT_RUNS_LIMIT }: { state: OverviewState; limit?: number }) {
  return (
    <section className="card" data-testid="landing-runs">
      <h2 style={{ margin: "0 0 10px" }}>Runs</h2>
      {state.kind === "loading" ? (
        <p className="muted" data-testid="landing-runs-loading" style={{ margin: 0 }}>
          Loading runs…
        </p>
      ) : state.kind !== "ready" ? (
        <div data-testid="landing-runs-problem">
          <OverviewProblem state={state} />
        </div>
      ) : recentRuns(state.project, limit).length === 0 ? (
        <p className="muted" data-testid="landing-runs-empty" style={{ margin: 0 }}>
          No runs yet.
        </p>
      ) : (
        <table style={{ width: "100%", borderCollapse: "collapse" }}>
          <thead>
            <tr className="muted" style={{ textAlign: "left", fontSize: 12 }}>
              <th style={{ padding: "4px 8px 4px 0" }}>Run</th>
              <th style={{ padding: "4px 8px 4px 0" }}>Work item</th>
              <th style={{ padding: "4px 8px 4px 0" }}>Phase</th>
              <th style={{ padding: "4px 8px 4px 0" }}>Claimed</th>
            </tr>
          </thead>
          <tbody>
            {recentRuns(state.project, limit).map((r) => (
              <tr key={r.name} data-testid="landing-run-row">
                <td style={{ padding: "4px 8px 4px 0" }}>
                  <a
                    href={`/runs/${encodeURIComponent(r.name)}${
                      r.workItem ? `?wi=${encodeURIComponent(r.workItem)}` : ""
                    }`}
                  >
                    {r.name}
                  </a>
                </td>
                <td style={{ padding: "4px 8px 4px 0" }}>
                  {r.workItem ? <code>{r.workItem}</code> : <span className="muted">—</span>}
                </td>
                <td style={{ padding: "4px 8px 4px 0" }}>
                  <span className="phase-chip" data-tone={phaseTone(r.phase)}>
                    {r.phase}
                  </span>
                </td>
                <td style={{ padding: "4px 8px 4px 0" }}>
                  {r.claimedAt ? (
                    new Date(r.claimedAt).toLocaleString()
                  ) : (
                    <span className="muted">—</span>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </section>
  );
}

export function StatsPanel({
  overview,
  workItems,
}: {
  overview: OverviewState;
  workItems: WorkItemsState;
}) {
  const project = overview.kind === "ready" ? overview.project : null;
  const items = workItems.kind === "ready" ? workItems.items : [];
  const phaseEntries = project ? Object.entries(project.phaseCounts ?? {}) : [];
  const issues = workItems.kind === "ready" ? partitionIssues(items) : null;
  const activity = lastActivity(project, items);
  // "—" ⇒ source unavailable/unknown; a real number (incl. 0) ⇒ authoritative (FR-I3).
  const dash = <span className="muted">—</span>;

  return (
    <section className="card" data-testid="landing-stats">
      <h2 style={{ margin: "0 0 10px" }}>Stats</h2>

      <div style={{ marginBottom: 10 }}>
        <div className="muted" style={{ fontSize: 12, marginBottom: 4 }}>Runs by phase</div>
        {overview.kind === "loading" ? (
          <span className="muted" data-testid="landing-stats-phase-loading">Loading…</span>
        ) : overview.kind !== "ready" ? (
          <span className="muted" data-testid="landing-stats-phase-unavailable">—</span>
        ) : phaseEntries.length === 0 ? (
          <span className="muted" data-testid="landing-stats-phase-empty">No runs.</span>
        ) : (
          <div style={{ display: "flex", gap: 8, flexWrap: "wrap" }}>
            {phaseEntries.map(([phase, n]) => (
              <span
                key={phase}
                className="phase-chip"
                data-tone={phaseTone(phase)}
                data-testid="landing-stats-phase-count"
              >
                {phase} · {n}
              </span>
            ))}
          </div>
        )}
      </div>

      <dl style={{ display: "grid", gridTemplateColumns: "auto auto", gap: "4px 16px", margin: 0 }}>
        <dt className="muted">Open issues</dt>
        <dd style={{ margin: 0 }} data-testid="landing-stats-open">
          {workItems.kind === "loading" ? "…" : issues ? issues.open : dash}
        </dd>
        <dt className="muted">Closed issues</dt>
        <dd style={{ margin: 0 }} data-testid="landing-stats-closed">
          {workItems.kind === "loading" ? "…" : issues ? issues.closed : dash}
        </dd>
        <dt className="muted">Last activity</dt>
        <dd style={{ margin: 0 }} data-testid="landing-stats-activity">
          {activity ? new Date(activity).toLocaleString() : dash}
        </dd>
      </dl>
    </section>
  );
}

export function LatestTicketsPanel({
  state,
  projectId,
  limit = DEFAULT_TICKETS_LIMIT,
}: {
  state: WorkItemsState;
  projectId: string;
  limit?: number;
}) {
  const issuesHref = `/projects/${encodeURIComponent(projectId)}/issues`;
  return (
    <section className="card" data-testid="landing-tickets">
      <div style={{ display: "flex", justifyContent: "space-between", alignItems: "baseline" }}>
        <h2 style={{ margin: "0 0 10px" }}>Latest Tickets</h2>
        <a href={issuesHref} data-testid="landing-tickets-viewall" style={{ fontSize: 13 }}>
          View all → Issues
        </a>
      </div>
      {state.kind === "loading" ? (
        <p className="muted" data-testid="landing-tickets-loading" style={{ margin: 0 }}>
          Loading tickets…
        </p>
      ) : state.kind === "not-available" ? (
        <p className="muted" data-testid="landing-tickets-unavailable" style={{ margin: 0 }}>
          Ticket read model not available yet on this deployment.
        </p>
      ) : state.kind === "error" ? (
        <p className="muted" data-testid="landing-tickets-error" style={{ margin: 0 }}>
          Tickets unavailable (HTTP {state.status || "network error"}).
        </p>
      ) : recentTickets(state.items, limit).length === 0 ? (
        <p className="muted" data-testid="landing-tickets-empty" style={{ margin: 0 }}>
          No tickets yet.
        </p>
      ) : (
        <ul style={{ listStyle: "none", margin: 0, padding: 0 }}>
          {recentTickets(state.items, limit).map((t) => (
            <li
              key={t.id}
              data-testid="landing-ticket-row"
              style={{ display: "flex", justifyContent: "space-between", gap: 8, padding: "4px 0" }}
            >
              <a href={`${issuesHref}?item=${encodeURIComponent(t.id)}`}>{t.title}</a>
              <span className="phase-chip" data-tone={t.state === "done" ? "idle" : "running"}>
                {STATE_LABELS[t.state]}
              </span>
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}

// ---------------------------------------------------------------------------
// Composed landing view — mounted by the S1 shell frame (ISI-3957).
// ---------------------------------------------------------------------------

export interface ProjectLandingProps {
  /** Project name/id — the overview slice key AND the work-items path param. */
  projectId: string;
  runsLimit?: number;
  ticketsLimit?: number;
}

export function ProjectLanding({ projectId, runsLimit, ticketsLimit }: ProjectLandingProps) {
  const overview = useOverviewProject(projectId);
  const workItems = useProjectWorkItems(projectId);
  // Memo the props objects so a re-render of one panel's data doesn't churn the others.
  const runsProps = useMemo(() => ({ state: overview, limit: runsLimit }), [overview, runsLimit]);
  return (
    <div
      data-testid="project-landing"
      style={{ display: "grid", gap: 16, gridTemplateColumns: "repeat(auto-fit, minmax(280px, 1fr))" }}
    >
      <RunsPanel {...runsProps} />
      <StatsPanel overview={overview} workItems={workItems} />
      <LatestTicketsPanel state={workItems} projectId={projectId} limit={ticketsLimit} />
    </div>
  );
}

export default ProjectLanding;
