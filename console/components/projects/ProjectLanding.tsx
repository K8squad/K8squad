"use client";

// components/projects/ProjectLanding.tsx — Project-Overview landing payload for the
// Project-Detail Workspace. Read-only panels mounted inside the S1 Landing frame
// (ISI-3957). Original S2 (ISI-3958) shipped Runs · Stats · Latest Tickets; the
// ISI-4399 S1 content pass adds the remaining Overview deltas ISI-4231 asked for:
//   • Stats split to the board's Open / In review / Done buckets (was open/closed).
//   • Team Composition — agent roster + role breakdown for the project's squad.
//   • Workspace — project identity (namespace / repo) + PVC status (honest "—"
//     until the ISI-4127 project-PVC read model lands).
// Story: ksquad/docs/bmad/stories/isi-3946-s2-landing-runs-stats-tickets.md
//
// Everything here is a REPROJECTION of reads the console already issues — no new
// back-end. Runs + the phase rollup come from GET /api/squad/overview (the same
// SquadOverview projection, sliced to ONE project by name); Latest Tickets + the
// ticket counts come from GET /api/projects/{id}/work-items via listWorkItems;
// Team Composition comes from GET /api/squad/agents (fleetlist.go FleetAgentList),
// filtered to the project's namespace (a squad IS a namespace).
//
// Each fetch owns its own async boundary so a slow or 501 read degrades ONLY its
// consumers (per-panel isolation). Fabrication discipline (FR-I3): a value whose
// source is UNAVAILABLE renders "—"; only an authoritative zero renders 0.

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

/**
 * One agent row from GET /api/squad/agents (fleetlist.go AgentListEntry). Live
 * status is intentionally NOT projected on the fleet LIST (that stays the per-agent
 * org read) — the composition summary is a roster + role rollup, not a status board.
 */
export interface SquadAgentEntry {
  id: string;
  name: string;
  namespace: string;
  runtime?: string;
  role?: string;
  model?: string;
  skillCount?: number;
}

/** Wire shape of GET /api/squad/agents (fleetlist.go FleetAgentList). */
export interface SquadAgentsData {
  agents: SquadAgentEntry[] | null;
  fleet?: boolean;
}

export type AgentsState =
  | { kind: "loading" }
  | { kind: "not-available" } // read model unhosted (501) or absent (404) — degrade honestly
  | { kind: "error"; status: number }
  | { kind: "ready"; agents: SquadAgentEntry[] };

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

/**
 * Board-bucket ticket rollup (ISI-4231 / ISI-4399 S1): the Overview reports the
 * three buckets the board thinks in — Done ≡ state "done"; In review ≡ state
 * "in_review"; Open ≡ everything still to do (backlog / todo / in_progress). The
 * five-value `state` enum is the authority (§13 derivation), so the split is a
 * pure fold over it — no separate stored counter to drift.
 */
export function ticketStats(items: WorkItem[]): { open: number; inReview: number; done: number } {
  let inReview = 0;
  let done = 0;
  for (const it of items) {
    if (it.state === "done") done += 1;
    else if (it.state === "in_review") inReview += 1;
  }
  return { open: items.length - inReview - done, inReview, done };
}

/**
 * Team-composition slice (ISI-4399 S1): the agents belonging to THIS project's
 * squad. "A squad is a namespace", so the project's overview `namespace` is the
 * join key — a fleet-admin caller sees every squad's agents on /api/squad/agents,
 * and this narrows to the one squad the project lives in. When the namespace is
 * unknown (overview not ready) the full roster is returned rather than fabricating
 * a filter, so the panel degrades to "the agents I can see" instead of empty.
 */
export function selectProjectAgents(
  agents: SquadAgentEntry[],
  namespace: string | null | undefined,
): SquadAgentEntry[] {
  if (!namespace) return agents;
  return agents.filter((a) => a.namespace === namespace);
}

/** Role → count rollup for the composition summary; a role-less agent counts as "unassigned". */
export function agentRoleBreakdown(agents: SquadAgentEntry[]): { role: string; count: number }[] {
  const counts = new Map<string, number>();
  for (const a of agents) {
    const role = a.role && a.role.trim() ? a.role : "unassigned";
    counts.set(role, (counts.get(role) ?? 0) + 1);
  }
  return [...counts.entries()]
    .sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]))
    .map(([role, count]) => ({ role, count }));
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
// Data hooks — each fetch is an independent async boundary.
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

/** Fetch the squad agent roster (GET-only, read-only). 501/404 ⇒ honest not-available. */
function useSquadAgents(): AgentsState {
  const [state, setState] = useState<AgentsState>({ kind: "loading" });
  useEffect(() => {
    let alive = true;
    setState({ kind: "loading" });
    fetch("/api/squad/agents", { headers: { accept: "application/json" } })
      .then(async (res) => {
        if (!alive) return;
        if (!res.ok) {
          setState(
            res.status === 501 || res.status === 404
              ? { kind: "not-available" }
              : { kind: "error", status: res.status },
          );
          return;
        }
        const data = (await res.json()) as SquadAgentsData;
        setState({ kind: "ready", agents: data.agents ?? [] });
      })
      .catch(() => {
        if (alive) setState({ kind: "error", status: 0 });
      });
    return () => {
      alive = false;
    };
  }, []);
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
  const stats = workItems.kind === "ready" ? ticketStats(items) : null;
  const activity = lastActivity(project, items);
  // "—" ⇒ source unavailable/unknown; a real number (incl. 0) ⇒ authoritative (FR-I3).
  const dash = <span className="muted">—</span>;
  const loading = workItems.kind === "loading";
  const cell = (value: number | undefined) => (loading ? "…" : stats ? value : dash);

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
        <dt className="muted">Open</dt>
        <dd style={{ margin: 0 }} data-testid="landing-stats-open">
          {cell(stats?.open)}
        </dd>
        <dt className="muted">In review</dt>
        <dd style={{ margin: 0 }} data-testid="landing-stats-inreview">
          {cell(stats?.inReview)}
        </dd>
        <dt className="muted">Done</dt>
        <dd style={{ margin: 0 }} data-testid="landing-stats-done">
          {cell(stats?.done)}
        </dd>
        <dt className="muted">Last activity</dt>
        <dd style={{ margin: 0 }} data-testid="landing-stats-activity">
          {activity ? new Date(activity).toLocaleString() : dash}
        </dd>
      </dl>
    </section>
  );
}

export function TeamCompositionPanel({
  agents,
  namespace,
}: {
  agents: AgentsState;
  /** The project's squad namespace (from the overview projection) — the roster filter key. */
  namespace: string | null | undefined;
}) {
  const roster = agents.kind === "ready" ? selectProjectAgents(agents.agents, namespace) : [];
  const breakdown = agentRoleBreakdown(roster);
  return (
    <section className="card" data-testid="landing-team">
      <h2 style={{ margin: "0 0 10px" }}>Team Composition</h2>
      {agents.kind === "loading" ? (
        <p className="muted" data-testid="landing-team-loading" style={{ margin: 0 }}>
          Loading team…
        </p>
      ) : agents.kind === "not-available" ? (
        <p className="muted" data-testid="landing-team-unavailable" style={{ margin: 0 }}>
          Agent roster not available yet on this deployment.
        </p>
      ) : agents.kind === "error" ? (
        <p className="muted" data-testid="landing-team-error" style={{ margin: 0 }}>
          Team unavailable (HTTP {agents.status || "network error"}).
        </p>
      ) : roster.length === 0 ? (
        <p className="muted" data-testid="landing-team-empty" style={{ margin: 0 }}>
          No agents in this squad yet.
        </p>
      ) : (
        <>
          <div data-testid="landing-team-count" style={{ marginBottom: 8 }}>
            <strong>{roster.length}</strong>{" "}
            <span className="muted">{roster.length === 1 ? "agent" : "agents"}</span>
          </div>
          <div style={{ display: "flex", gap: 8, flexWrap: "wrap" }}>
            {breakdown.map((b) => (
              <span key={b.role} className="phase-chip" data-testid="landing-team-role">
                {b.role} · {b.count}
              </span>
            ))}
          </div>
        </>
      )}
    </section>
  );
}

export function WorkspacePanel({ overview }: { overview: OverviewState }) {
  const project = overview.kind === "ready" ? overview.project : null;
  const dash = <span className="muted">—</span>;
  const repoUrl = project?.repoUrl?.trim() || "";
  return (
    <section className="card" data-testid="landing-workspace">
      <h2 style={{ margin: "0 0 10px" }}>Workspace</h2>
      {overview.kind === "loading" ? (
        <p className="muted" data-testid="landing-workspace-loading" style={{ margin: 0 }}>
          Loading workspace…
        </p>
      ) : overview.kind !== "ready" ? (
        <div data-testid="landing-workspace-problem">
          <OverviewProblem state={overview} />
        </div>
      ) : (
        <dl style={{ display: "grid", gridTemplateColumns: "auto 1fr", gap: "4px 16px", margin: 0 }}>
          <dt className="muted">Namespace</dt>
          <dd style={{ margin: 0 }} data-testid="landing-workspace-namespace">
            {project?.namespace ? <code>{project.namespace}</code> : dash}
          </dd>
          <dt className="muted">Repository</dt>
          <dd style={{ margin: 0, overflowWrap: "anywhere" }} data-testid="landing-workspace-repo">
            {repoUrl ? (
              <a href={repoUrl} target="_blank" rel="noreferrer noopener">
                {repoUrl}
              </a>
            ) : (
              dash
            )}
          </dd>
          <dt className="muted">Storage (PVC)</dt>
          {/* PVC status has no read model yet (ISI-4127) — degrade honestly, never fabricate. */}
          <dd style={{ margin: 0 }} data-testid="landing-workspace-pvc">
            <span className="muted" title="Project-PVC read model not yet available (ISI-4127)">
              — not reported
            </span>
          </dd>
        </dl>
      )}
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
  const agents = useSquadAgents();
  const projectNamespace = overview.kind === "ready" ? overview.project?.namespace : undefined;
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
      <TeamCompositionPanel agents={agents} namespace={projectNamespace} />
      <WorkspacePanel overview={overview} />
    </div>
  );
}

export default ProjectLanding;
