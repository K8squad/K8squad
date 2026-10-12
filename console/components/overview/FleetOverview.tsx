"use client";

// components/overview/FleetOverview.tsx — the global/fleet overview, recomposed per
// Mock A (ISI-5708 WS-3; parent plan ISI-5697 rev2, approved design ISI-5699). The
// Paperclip-style control room, top to bottom:
//
//   ① stat band        — agent states first (running / idle / blocked), then projects
//                         and 30-day tokens — Henrik's "global stats first" ask.
//   ② time-series row  — fleet tickets-by-status (stacked area) + in-window runs-by-
//                         status (h-bars), from GET /api/squad/overview/series (WS-1c).
//   ③ GitHub strip     — fleet issue/PR/check roll-up + 7-day merge sparkline, from
//                         GET /api/squad/github (WS-1b).
//   ④ agent-card grid  — the CURRENT-runs surface: one AgentRunCard per agent from
//                         GET /api/squad/agents/activity (WS-1a + WS-2), replacing the
//                         opaque intake-<uuid> run list with named agents + CLASSIFIED
//                         thinking (never the raw [run]/[tool] stream — ISI-5695/5696).
//   ⑤ project jump row — compact per-project rows: high-level details (runs mix, open
//                         PRs) + deep links into each project overview and its issues.
//
// Every panel owns its own async boundary and loading/empty/error state, so a slow or
// unrolled read degrades ONLY its own panel — never the page (degrade-don't-blank,
// ISI-4229). Fabrication discipline (FR-I3): an unavailable source renders "—", an
// authoritative zero renders "0", never an invented value. The page itself still gates
// on the core squad-overview read (auth / no-team / not-wired) exactly as before.

import { useEffect, useMemo, useState } from "react";
import {
  classifyOverviewStatus,
  phaseTone,
  UNASSIGNED_PROJECT_BUCKET,
  type SquadOverviewData,
} from "@/components/SquadOverview";
import { type OverviewProject } from "@/components/projects/ProjectLanding";
import { encodeProjectId } from "@/lib/projectId";
import {
  StatBand,
  StatTile,
  PanelCard,
  RunStatusMixBar,
  AgentRunCard,
  AgentRunCardGrid,
  StackedAreaChart,
  HBarChart,
} from "@/components/overview";
import {
  fetchAgentActivity,
  type AgentActivityState,
} from "@/lib/overview/agentActivity";
import {
  fetchFleetSeries,
  type FleetOverviewSeries,
} from "@/lib/overview/fleetSeries";
import {
  areaXLabels,
  formatTokens,
  toAreaSeries,
  toRunBars,
  type SeriesState,
} from "@/lib/overview/projectSeries";
import {
  fetchGithubSummary,
  selectRepoStat,
  repoCount,
  type GithubSummaryState,
} from "@/lib/overview/githubSummary";
import "@/components/overview/overview.css";
import "@/components/overview/dashboard.css";

const AGENT_SKELETONS = 6; // placeholder cards while the activity read is in flight

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

/** Flatten every project's runs, newest claim first. */
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

/** "2026-09-16T15:04:05Z" → "2026-09-16 15:04" — compact, locale-neutral. */
export function formatWhen(iso: string | null): string {
  if (!iso) return "—";
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return "—";
  return new Date(t).toISOString().slice(0, 16).replace("T", " ");
}

/** Total runs a project reports across every phase (its headline figure in the jump row). */
export function totalRuns(project: OverviewProject): number {
  return Object.values(project.phaseCounts ?? {}).reduce((a, b) => a + (b ?? 0), 0);
}

/** Split the fixed overview projection (ISI-4570) into real projects and the synthetic
 * "Unassigned/other" bucket. The bucket is not a project: no row, no fan-out reads. */
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
// Data hooks — each an independent async boundary (degrade-don't-blank).
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

function useAgentActivity(): AgentActivityState {
  const [state, setState] = useState<AgentActivityState>({ kind: "loading" });
  useEffect(() => {
    const ctrl = new AbortController();
    fetchAgentActivity(ctrl.signal).then((s) => {
      if (!ctrl.signal.aborted) setState(s);
    });
    return () => ctrl.abort();
  }, []);
  return state;
}

function useFleetSeries(): SeriesState {
  const [state, setState] = useState<SeriesState>({ kind: "loading" });
  useEffect(() => {
    const ctrl = new AbortController();
    fetchFleetSeries("30d", ctrl.signal).then((s) => {
      if (!ctrl.signal.aborted) setState(s);
    });
    return () => ctrl.abort();
  }, []);
  return state;
}

function useGithubSummary(): GithubSummaryState {
  const [state, setState] = useState<GithubSummaryState>({ kind: "loading" });
  useEffect(() => {
    const ctrl = new AbortController();
    fetchGithubSummary(ctrl.signal).then((s) => {
      if (!ctrl.signal.aborted) setState(s);
    });
    return () => ctrl.abort();
  }, []);
  return state;
}

// ---------------------------------------------------------------------------
// Panels
// ---------------------------------------------------------------------------

/** ② Time-series row: fleet tickets-by-status stacked area + in-window runs-by-status h-bars. */
function TimeSeriesRow({ state }: { state: SeriesState }) {
  const series: FleetOverviewSeries | null = state.kind === "ready" ? state.series : null;
  const tickets = series?.ticketsByStatus;
  const runs = series?.runsByStatus;

  const ticketsPanelState =
    state.kind === "loading"
      ? "loading"
      : state.kind === "error"
        ? "error"
        : tickets?.available
          ? "ready"
          : "empty";
  const runsPanelState =
    state.kind === "loading"
      ? "loading"
      : state.kind === "error"
        ? "error"
        : runs?.available
          ? "ready"
          : "empty";

  const snapshots = tickets?.snapshots ?? [];

  return (
    <div className="ov-charts-row" data-testid="fleet-series">
      <PanelCard
        title="Tickets by status · 30d"
        state={ticketsPanelState}
        emptyLabel="Ticket history not available yet on this deployment."
        errorLabel="Couldn't load the ticket time series."
      >
        <StackedAreaChart
          series={toAreaSeries(snapshots)}
          xLabels={areaXLabels(snapshots)}
          ariaLabel="Fleet tickets by status over the last 30 days"
        />
      </PanelCard>
      <PanelCard
        title="Runs by status · 30d"
        state={runsPanelState}
        emptyLabel="No runs in this window."
        errorLabel="Couldn't load the runs breakdown."
      >
        <HBarChart
          items={toRunBars(runs?.byPhase ?? {})}
          ariaLabel="Fleet runs by status in the last 30 days"
        />
      </PanelCard>
    </div>
  );
}

/** A tiny inline 7-day merge sparkline (bars, oldest→newest). Pure SVG, no deps. */
function MergeSparkline({ values }: { values: number[] }) {
  const max = Math.max(1, ...values);
  return (
    <span className="ov-sparkline" role="img" aria-label={`PRs merged per day, last 7 days: ${values.join(", ")}`}>
      {values.map((v, i) => (
        <span
          key={i}
          className="ov-sparkline__bar"
          style={{ height: `${Math.round((Math.max(0, v) / max) * 100)}%` }}
        />
      ))}
    </span>
  );
}

/** ③ GitHub summary strip: fleet issue/PR/check roll-up + merge trend. */
function GithubStrip({ state }: { state: GithubSummaryState }) {
  const panelState =
    state.kind === "loading"
      ? "loading"
      : state.kind === "error"
        ? "error"
        : state.kind === "not-available"
          ? "empty"
          : "ready";
  const t = state.kind === "ready" ? state.data.totals : null;
  const spark = state.kind === "ready" ? state.data.mergeSparkline7d : [];

  return (
    <PanelCard
      title="GitHub"
      state={panelState}
      emptyLabel="GitHub summary not available on this deployment."
      errorLabel={`GitHub summary unavailable (HTTP ${state.kind === "error" ? state.status || "network error" : ""}).`}
    >
      {t && (
        <div className="ov-github-strip" data-testid="fleet-github">
          <GithubStat label="Issues open" value={t.issuesOpen} />
          <GithubStat label="Assigned" value={t.issuesAssigned} />
          <GithubStat label="PRs open" value={t.prsOpened} />
          <GithubStat label="Merged" value={t.prsMerged} />
          <GithubStat label="Reviewed" value={t.prsReviewed} />
          <GithubStat label="Checks red" value={t.checksRed} tone={t.checksRed > 0 ? "blocked" : undefined} />
          <div className="ov-github-strip__spark">
            <span className="ov-github-stat__label">Merged · 7d</span>
            <MergeSparkline values={spark} />
          </div>
        </div>
      )}
    </PanelCard>
  );
}

function GithubStat({ label, value, tone }: { label: string; value: number; tone?: "blocked" }) {
  return (
    <div className="ov-github-stat" data-tone={tone} data-testid="fleet-github-stat">
      <span className="ov-github-stat__value">{value}</span>
      <span className="ov-github-stat__label">{label}</span>
    </div>
  );
}

/** ④ Current-runs agent-card grid (the Paperclip-style named-agent surface). */
function AgentGrid({ state }: { state: AgentActivityState }) {
  if (state.kind === "loading") {
    return (
      <PanelCard title="Current agent runs" state="ready">
        <AgentRunCardGrid>
          {Array.from({ length: AGENT_SKELETONS }, (_, i) => (
            <AgentRunCard key={i} loading />
          ))}
        </AgentRunCardGrid>
      </PanelCard>
    );
  }
  const panelState =
    state.kind === "error"
      ? "error"
      : state.kind === "not-available"
        ? "empty"
        : state.data.agents.length === 0
          ? "empty"
          : "ready";
  return (
    <PanelCard
      title="Current agent runs"
      action={{ label: "View all runs →", href: "/runs" }}
      state={panelState}
      emptyLabel={
        state.kind === "not-available"
          ? "Agent activity read model not available yet on this deployment."
          : "No agents yet."
      }
      errorLabel={`Agent activity unavailable (HTTP ${state.kind === "error" ? state.status || "network error" : ""}).`}
    >
      {state.kind === "ready" && (
        <div data-testid="fleet-agent-grid">
          <AgentRunCardGrid>
            {state.data.agents.map((a) => (
              <AgentRunCard key={a.agentId || a.name} agent={a} />
            ))}
          </AgentRunCardGrid>
        </div>
      )}
    </PanelCard>
  );
}

/** ⑤ Per-project jump row: high-level details + deep links to each project + its issues. */
function ProjectJumpRow({
  projects,
  github,
}: {
  projects: OverviewProject[];
  github: GithubSummaryState;
}) {
  const summary = github.kind === "ready" ? github.data : null;
  return (
    <PanelCard title="Projects" state={projects.length === 0 ? "empty" : "ready"} emptyLabel="No projects yet.">
      <ul className="ov-project-jump" data-testid="fleet-projects">
        {projects.map((p) => {
          // Reuse the WS-4 per-project selector (selectRepoStat + repoCount): the open-PR
          // count is authoritative when the repo is mirrored, "—" otherwise (FR-I3).
          const openPrs = repoCount(selectRepoStat(summary, p.repoUrl), "prs");
          return (
            <li key={`${p.namespace}/${p.name}`} className="ov-project-jump__row" data-testid="fleet-project-jump">
              <div className="ov-project-jump__id">
                <a className="ov-project-jump__name" href={`/projects/${encodeProjectId(p.name)}`}>
                  {p.name}
                </a>
                <span className="muted ov-project-jump__ns">{p.namespace}</span>
              </div>
              <RunStatusMixBar counts={mixFromPhaseCounts(p.phaseCounts)} />
              <div className="ov-project-jump__metrics">
                <span>{totalRuns(p)} runs</span>
                <span data-testid="fleet-project-prs">
                  {typeof openPrs === "number" ? `${openPrs} open PR${openPrs === 1 ? "" : "s"}` : "— PRs"}
                </span>
              </div>
              <a
                className="ov-project-jump__issues"
                href={`/projects/${encodeProjectId(p.name)}/issues`}
                data-testid="fleet-project-issues-link"
              >
                Issues →
              </a>
            </li>
          );
        })}
      </ul>
    </PanelCard>
  );
}

// ---------------------------------------------------------------------------
// Assembly
// ---------------------------------------------------------------------------

export function FleetOverview() {
  const overview = useSquadOverview();
  const activity = useAgentActivity();
  const series = useFleetSeries();
  const github = useGithubSummary();

  const { projects } = useMemo(
    () => partitionProjects(overview.kind === "ready" ? overview.data.projects : null),
    [overview],
  );

  if (overview.kind === "loading") {
    return (
      <div className="ov-dashboard" data-testid="fleet-loading" aria-busy="true">
        <StatBand>
          <StatTile label="Running" value="" loading />
          <StatTile label="Idle" value="" loading />
          <StatTile label="Blocked" value="" loading />
          <StatTile label="Projects" value="" loading />
          <StatTile label="Tokens · 30d" value="" loading />
        </StatBand>
        <PanelCard title="Current agent runs" state="loading" />
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

  const counts = activity.kind === "ready" ? activity.data.counts : null;
  const tokens =
    series.kind === "ready" && series.series.tokens?.available
      ? formatTokens(series.series.tokens.total)
      : "—";

  return (
    <div className="ov-dashboard" data-testid="fleet-overview">
      {/* ① stat band — agent states first (Henrik's "global stats first"). */}
      <StatBand>
        <StatTile
          label="Running"
          value={counts ? counts.running : "—"}
          loading={activity.kind === "loading"}
          tone="running"
          glyph="⚡"
          sub="agents"
        />
        <StatTile
          label="Idle"
          value={counts ? counts.idle : "—"}
          loading={activity.kind === "loading"}
          sub="agents"
        />
        <StatTile
          label="Blocked"
          value={counts ? counts.blocked : "—"}
          loading={activity.kind === "loading"}
          tone={counts && counts.blocked > 0 ? "blocked" : undefined}
          sub="agents"
        />
        <StatTile
          label="Projects"
          value={projects.length}
          sub={overview.data.fleet ? "all squads" : overview.data.team.name}
        />
        <StatTile
          label="Tokens · 30d"
          value={tokens}
          loading={series.kind === "loading"}
          sub={tokens === "—" ? "no runs report" : "consumed"}
        />
      </StatBand>

      {/* ② time-series row */}
      <TimeSeriesRow state={series} />

      {/* ③ GitHub summary strip */}
      <GithubStrip state={github} />

      {/* ④ current-runs agent-card grid */}
      <AgentGrid state={activity} />

      {/* ⑤ per-project jump row */}
      <ProjectJumpRow projects={projects} github={github} />
    </div>
  );
}
