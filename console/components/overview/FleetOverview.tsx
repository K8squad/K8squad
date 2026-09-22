"use client";

// components/overview/FleetOverview.tsx — Frame 01, the fleet control-room global overview
// (ISI-4507 S2; DESIGN-SPEC-ISI-4505 §3/§4, mock 01-global-overview-fleet). Replaces the old
// SquadOverview as the post-onboarding Overview surface (mounted by OverviewSwitch for BOTH
// / and /overview, preserving the D5/ISI-3674 onboarding gate). Composes the S1 primitives
// (ISI-4506) and reads the SAME wire shapes as S3 (ProjectOverviewDashboard):
//   • GET /api/squad/overview — every project with live runs + phaseCounts (the ONLY runs source);
//   • GET /api/squad/agents — the agents-active tile;
//   • GET /api/projects/{id}/work-items per project (allSettled) — the open-tickets tile,
//     per-card open counts, and the Recent Ticket Work panel (rows deep-link to the
//     ISI-4231 issue detail route, keyed by each item's own projectId);
//   • the S4 series read (ISI-4509) per project (allSettled) — the tokens tile sums every
//     available project's 30-day total; "—" when no project reports one (FR-I3: never invent).
//
// Every panel owns its async boundary and its loading/empty/error state via PanelCard, so a
// slow or unrolled read degrades ONLY its own tile — never the page (degrade-don't-blank,
// ISI-4229). Same-named projects from different squads (fleet mode) are disambiguated in the
// card by their namespace; per-card links stay name-based until ISI-3967's qualified route.

import { useEffect, useMemo, useState } from "react";
import {
  classifyOverviewStatus,
  phaseTone,
  UNASSIGNED_PROJECT_BUCKET,
  type SquadOverviewData,
} from "@/components/SquadOverview";
import {
  lastActivity,
  recentRuns,
  recentTickets,
  ticketStats,
  type OverviewProject,
  type SquadAgentEntry,
  type SquadAgentsData,
} from "@/components/projects/ProjectLanding";
import { ApiError, listWorkItems } from "@/lib/tickets/api";
import { STATE_LABELS, type WorkItem } from "@/lib/tickets/types";
import { encodeProjectId } from "@/lib/projectId";
import { StatBand, StatTile, PanelCard, RunStatusMixBar } from "@/components/overview";
import { fetchProjectSeries, formatTokens } from "@/lib/overview/projectSeries";
import "@/components/overview/overview.css";
import "@/components/overview/dashboard.css";

const CARD_RUNS_LIMIT = 3; // current runs listed inside a project card
const PANEL_ROWS_LIMIT = 6; // rows in the recent-tickets / live-runs panels

// ---------------------------------------------------------------------------
// Pure projections (exported for unit tests — no fetch, no DOM).
// ---------------------------------------------------------------------------

/** Fold a project's phaseCounts into the mix-bar's four states (phaseTone is the authority). */
export function mixFromPhaseCounts(phaseCounts: Record<string, number>): {
  running: number;
  paused: number;
  blocked: number;
  idle: number;
} {
  const mix = { running: 0, paused: 0, blocked: 0, idle: 0 };
  for (const [phase, n] of Object.entries(phaseCounts ?? {})) {
    mix[phaseTone(phase) as keyof typeof mix] += n ?? 0;
  }
  return mix;
}

/** Runs whose phase is live work (phaseTone "running") across the whole fleet. */
export function activeRunCount(projects: OverviewProject[] | null): number {
  let n = 0;
  for (const p of projects ?? []) {
    for (const r of p.runs ?? []) {
      if (phaseTone(r.phase) === "running") n += 1;
    }
  }
  return n;
}

/** Flatten every project's runs, newest claim first (the Live Runs feed's source). */
export function fleetRuns(
  projects: OverviewProject[] | null,
  limit: number,
): { project: OverviewProject; name: string; phase: string; claimedAt?: string | null; workItem?: string }[] {
  const all = (projects ?? []).flatMap((p) =>
    (p.runs ?? []).map((r) => ({ project: p, ...r })),
  );
  return [...all]
    .sort(
      (a, b) =>
        (Date.parse(b.claimedAt ?? "") || 0) - (Date.parse(a.claimedAt ?? "") || 0),
    )
    .slice(0, limit);
}

/** "2026-09-16T15:04:05Z" → "2026-09-16 15:04" — compact, locale-neutral, no relative-time drift in tests. */
export function formatWhen(iso: string | null): string {
  if (!iso) return "—";
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return "—";
  return new Date(t).toISOString().slice(0, 16).replace("T", " ");
}

/** Split the fixed overview projection (ISI-4570) into real projects and the synthetic
 * "Unassigned/other" bucket. The bucket is not a project: no card, no fan-out reads — its
 * runs surface as a count on the Live Agent Runs panel (plan OQ1). */
export function partitionProjects(projects: OverviewProject[] | null): {
  projects: OverviewProject[];
  unassigned: OverviewProject | null;
} {
  const all = projects ?? [];
  return {
    projects: all.filter((p) => p.name !== UNASSIGNED_PROJECT_BUCKET),
    unassigned: all.find((p) => p.name === UNASSIGNED_PROJECT_BUCKET) ?? null,
  };
}

// ---------------------------------------------------------------------------
// Data hooks — each an independent async boundary (S3's discipline, verbatim shapes).
// ---------------------------------------------------------------------------

type OverviewState =
  | { kind: "loading" }
  | { kind: "unauthenticated" }
  | { kind: "no-team" }
  | { kind: "not-wired" }
  | { kind: "error"; status: number }
  | { kind: "ready"; data: SquadOverviewData };

function useSquadOverview(): OverviewState {
  const [state, setState] = useState<OverviewState>({ kind: "loading" });
  useEffect(() => {
    let alive = true;
    fetch("/api/squad/overview", { headers: { accept: "application/json" } })
      .then(async (res) => {
        if (!alive) return;
        if (!res.ok) {
          setState(classifyOverviewStatus(res.status) as OverviewState);
          return;
        }
        setState({ kind: "ready", data: (await res.json()) as SquadOverviewData });
      })
      .catch(() => alive && setState({ kind: "error", status: 0 }));
    return () => {
      alive = false;
    };
  }, []);
  return state;
}

type AgentsState =
  | { kind: "loading" }
  | { kind: "not-available" }
  | { kind: "error"; status: number }
  | { kind: "ready"; agents: SquadAgentEntry[] };

function useSquadAgents(): AgentsState {
  const [state, setState] = useState<AgentsState>({ kind: "loading" });
  useEffect(() => {
    let alive = true;
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
      .catch(() => alive && setState({ kind: "error", status: 0 }));
    return () => {
      alive = false;
    };
  }, []);
  return state;
}

type WorkItemsState =
  | { kind: "loading" }
  | { kind: "not-available" }
  | { kind: "error"; status: number }
  | { kind: "ready"; byProject: Map<string, WorkItem[]> };

/** Per-project work-items fan-out (allSettled): one project's failure never sinks the fleet —
 * a 501/404 everywhere degrades the tiles honestly, everything else surfaces per-panel error. */
function useFleetWorkItems(projects: OverviewProject[]): WorkItemsState {
  const [state, setState] = useState<WorkItemsState>({ kind: "loading" });
  useEffect(() => {
    let alive = true;
    if (projects.length === 0) {
      setState({ kind: "ready", byProject: new Map() });
      return;
    }
    Promise.allSettled(projects.map((p) => listWorkItems(p.name))).then((results) => {
      if (!alive) return;
      const byProject = new Map<string, WorkItem[]>();
      let notWired = 0;
      let failed = 0;
      results.forEach((r, i) => {
        if (r.status === "fulfilled") {
          byProject.set(projects[i].name, r.value);
        } else {
          const status = r.reason instanceof ApiError ? r.reason.status : 0;
          if (status === 501 || status === 404) notWired += 1;
          else failed += 1;
        }
      });
      if (byProject.size === 0 && notWired > 0) setState({ kind: "not-available" });
      else if (byProject.size === 0 && failed > 0) setState({ kind: "error", status: 0 });
      else setState({ kind: "ready", byProject });
    });
    return () => {
      alive = false;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps -- projects identity is stable per overview fetch
  }, [projects]);
  return state;
}

type TokensState =
  | { kind: "loading" }
  | { kind: "not-available" }
  | { kind: "ready"; total: number };

/** Sum every available project's 30-day token total (S4 series, allSettled). No project
 * reporting tokens ⇒ "—" (FR-I3: unavailable renders "—", never an invented 0). */
function useFleetTokens(projects: OverviewProject[]): TokensState {
  const [state, setState] = useState<TokensState>({ kind: "loading" });
  useEffect(() => {
    let alive = true;
    if (projects.length === 0) {
      setState({ kind: "not-available" });
      return;
    }
    Promise.allSettled(projects.map((p) => fetchProjectSeries(p.name, "30d"))).then(
      (results) => {
        if (!alive) return;
        let total = 0;
        let any = false;
        for (const r of results) {
          if (r.status === "fulfilled" && r.value.kind === "ready") {
            const tokens = r.value.series.tokens;
            if (tokens?.available && typeof tokens.total === "number") {
              total += tokens.total;
              any = true;
            }
          }
        }
        setState(any ? { kind: "ready", total } : { kind: "not-available" });
      },
    );
    return () => {
      alive = false;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps -- projects identity is stable per overview fetch
  }, [projects]);
  return state;
}

// ---------------------------------------------------------------------------
// Panels
// ---------------------------------------------------------------------------

function ProjectCard({
  project,
  items,
}: {
  project: OverviewProject;
  items: WorkItem[] | null;
}) {
  const runs = recentRuns(project, CARD_RUNS_LIMIT);
  const mix = mixFromPhaseCounts(project.phaseCounts);
  const open = items ? ticketStats(items).open : null;
  const totalRuns = Object.values(project.phaseCounts ?? {}).reduce((a, b) => a + (b ?? 0), 0);
  return (
    <a
      className="ov-fleet-card"
      href={`/projects/${encodeProjectId(project.name)}`}
      data-testid="fleet-project-card"
    >
      <div className="ov-fleet-card__head">
        <h3>{project.name}</h3>
        <span className="muted ov-fleet-card__ns">{project.namespace}</span>
      </div>
      <div className="ov-fleet-card__metrics">
        <span>{totalRuns} runs</span>
        <span>{open === null ? "tickets …" : `${open} open`}</span>
        <span>active {formatWhen(lastActivity(project, items ?? []))}</span>
      </div>
      <RunStatusMixBar counts={mix} />
      {runs.length > 0 ? (
        <ul className="ov-thinking" data-testid="fleet-card-runs">
          {runs.map((r) => (
            <li key={r.name} className="ov-thinking__row">
              <div className="ov-thinking__head">
                <span className="muted" title={r.workItem ?? undefined}>
                  {r.name}
                </span>
                <span className="phase-chip" data-tone={phaseTone(r.phase)}>
                  {r.phase}
                </span>
              </div>
            </li>
          ))}
        </ul>
      ) : (
        <p className="muted" style={{ margin: "8px 0 0" }}>
          No runs.
        </p>
      )}
    </a>
  );
}

function RecentTicketWorkPanel({ state }: { state: WorkItemsState }) {
  return (
    <PanelCard
      title="Recent Ticket Work"
      state={
        state.kind === "loading"
          ? "loading"
          : state.kind === "error"
            ? "error"
            : state.kind === "not-available"
              ? "empty"
              : "ready"
      }
      emptyLabel="Ticket read model not available yet on this deployment."
      errorLabel={`Tickets unavailable (HTTP ${state.kind === "error" ? state.status || "network error" : ""}).`}
    >
      {state.kind === "ready" ? (
        (() => {
          const merged = recentTickets(
            [...state.byProject.values()].flat(),
            PANEL_ROWS_LIMIT,
          );
          if (merged.length === 0) return <p className="muted" style={{ margin: 0 }}>No tickets yet.</p>;
          return (
            <ul className="ov-list" data-testid="fleet-recent-tickets">
              {merged.map((t) => (
                <li key={`${t.projectId}/${t.id}`} className="ov-list__row">
                  <a
                    href={`/projects/${encodeURIComponent(t.projectId)}/issues/${encodeURIComponent(t.id)}`}
                    data-testid="fleet-recent-ticket-link"
                  >
                    {t.title}
                  </a>
                  <span className="ov-fleet-ticket__meta">
                    <span className="muted">{t.projectId}</span>
                    <span className="phase-chip" data-tone={t.state === "done" ? "idle" : "running"}>
                      {STATE_LABELS[t.state]}
                    </span>
                  </span>
                </li>
              ))}
            </ul>
          );
        })()
      ) : null}
    </PanelCard>
  );
}

function LiveRunsPanel({
  projects,
  unassigned,
}: {
  projects: OverviewProject[] | null;
  unassigned: OverviewProject | null;
}) {
  const runs = fleetRuns(projects, PANEL_ROWS_LIMIT);
  const unassignedCount = (unassigned?.runs ?? []).length;
  return (
    <PanelCard
      title="Live Agent Runs"
      action={{ label: "View all runs →", href: "/runs" }}
      state={
        projects === null
          ? "loading"
          : runs.length === 0 && unassignedCount === 0
            ? "empty"
            : "ready"
      }
      emptyLabel="No agent runs."
    >
      <ul className="ov-thinking" data-testid="fleet-live-runs">
        {runs.map((r) => (
          <li key={`${r.project.name}/${r.name}`} className="ov-thinking__row">
            {/* ISI-4565: let the user open a run's detail directly from the
               overview instead of routing through the full runs list. */}
            <a
              className="ov-thinking__link"
              href={`/runs/${encodeURIComponent(r.name)}`}
              data-testid="fleet-run-link"
            >
              <div className="ov-thinking__head">
                <span title={r.workItem ?? undefined}>{r.name}</span>
                <span className="phase-chip" data-tone={phaseTone(r.phase)}>
                  {r.phase}
                </span>
              </div>
              <span className="muted" style={{ fontSize: 12 }}>
                {r.project.name} · {formatWhen(r.claimedAt ?? null)}
              </span>
            </a>
          </li>
        ))}
      </ul>
      {unassignedCount > 0 && (
        <p className="muted" style={{ fontSize: 12, margin: "8px 0 0" }} data-testid="fleet-unassigned-runs">
          + {unassignedCount} unassigned {unassignedCount === 1 ? "run" : "runs"} (no resolvable project)
        </p>
      )}
    </PanelCard>
  );
}

// ---------------------------------------------------------------------------
// Assembly
// ---------------------------------------------------------------------------

export function FleetOverview() {
  const overview = useSquadOverview();
  const agents = useSquadAgents();
  // Split the synthetic unassigned bucket out of the project list (ISI-4570): cards, the
  // Projects tile, and the per-project fan-out reads only ever see REAL projects; the bucket's
  // runs surface as a count on the Live Agent Runs panel. Stable identity per overview fetch —
  // the fan-out hooks key on it.
  const { projects, unassigned } = useMemo(
    () =>
      partitionProjects(overview.kind === "ready" ? overview.data.projects : null),
    [overview],
  );
  const workItems = useFleetWorkItems(projects);
  const tokens = useFleetTokens(projects);

  if (overview.kind === "loading") {
    return (
      <div className="ov-dashboard" data-testid="fleet-loading" aria-busy="true">
        <StatBand>
          <StatTile label="Active Runs" value="" loading />
          <StatTile label="Projects" value="" loading />
          <StatTile label="Open Tickets" value="" loading />
          <StatTile label="Agents" value="" loading />
          <StatTile label="Tokens · 30d" value="" loading />
        </StatBand>
        <PanelCard title="Projects" state="loading" />
      </div>
    );
  }

  if (overview.kind === "unauthenticated") {
    return <p className="muted" data-testid="fleet-unauthenticated">Sign in to see the fleet overview.</p>;
  }
  if (overview.kind === "no-team") {
    return <p className="muted" data-testid="fleet-no-team">No team is connected yet — complete setup to see the fleet.</p>;
  }
  if (overview.kind === "not-wired") {
    return (
      <p className="muted" data-testid="fleet-not-wired">
        The overview read model is not available yet on this deployment.
      </p>
    );
  }
  if (overview.kind === "error") {
    return (
      <p className="muted" data-testid="fleet-error">
        Fleet overview unavailable (HTTP {overview.status || "network error"}).
      </p>
    );
  }

  const openTickets =
    workItems.kind === "ready"
      ? ticketStats([...workItems.byProject.values()].flat()).open
      : null;

  return (
    <div className="ov-dashboard" data-testid="fleet-overview">
      <StatBand>
        <StatTile
          label="Active Runs"
          value={activeRunCount(overview.data.projects)}
          tone="running"
          sub="fleet-wide"
          glyph="⚡"
        />
        <StatTile
          label="Projects"
          value={projects.length}
          sub={overview.data.fleet ? "all squads" : overview.data.team.name}
        />
        <StatTile
          label="Open Tickets"
          value={openTickets ?? "—"}
          loading={workItems.kind === "loading"}
          sub={openTickets === null ? "read model unavailable" : "across projects"}
        />
        <StatTile
          label="Agents"
          value={agents.kind === "ready" ? agents.agents.length : "—"}
          loading={agents.kind === "loading"}
          sub={agents.kind === "ready" ? "in squad" : "unavailable"}
        />
        <StatTile
          label="Tokens · 30d"
          value={tokens.kind === "ready" ? formatTokens(tokens.total) : "—"}
          loading={tokens.kind === "loading"}
          sub={tokens.kind === "ready" ? "consumed" : "no project reports"}
        />
      </StatBand>

      <div className="ov-fleet-grid" data-testid="fleet-projects">
        {projects.map((p) => (
          <ProjectCard
            key={`${p.namespace}/${p.name}`}
            project={p}
            items={workItems.kind === "ready" ? (workItems.byProject.get(p.name) ?? []) : null}
          />
        ))}
        {projects.length === 0 && (
          <p className="muted">No projects yet.</p>
        )}
      </div>

      <div className="ov-bottom-row">
        <RecentTicketWorkPanel state={workItems} />
        <LiveRunsPanel projects={projects} unassigned={unassigned} />
      </div>
    </div>
  );
}
