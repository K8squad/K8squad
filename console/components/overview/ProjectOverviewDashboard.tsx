"use client";

// components/overview/ProjectOverviewDashboard.tsx — Frame 02, the project-filtered overview
// dashboard (ISI-4508 S3; DESIGN-SPEC-ISI-4505 §3/§4, mock 02-project-overview-dashboard).
// Replaces the old project Landing at the bare project root. Composes the S1 primitives
// (ISI-4506) and reads:
//   • the S4 time-series endpoint (ISI-4509) for tickets-over-time, runs-by-status, token sum —
//     degrades to honest empty charts when unrolled on this deployment;
//   • GET /api/squad/overview (sliced to this project) for live runs + active-run count;
//   • GET /api/projects/{id}/work-items for open-issue count + Latest tickets;
//   • GET /api/squad/agents (namespace-filtered) for the agents-active tile.
//
// Every panel owns its async boundary and its loading/empty/error state via PanelCard, so a slow
// or unrolled read degrades ONLY its own tile — never the page (degrade-don't-blank, ISI-4229).
// Fabrication discipline (FR-I3): an UNAVAILABLE source renders "—", only an authoritative zero
// renders 0. The latest-thinking snippet (design OQ2, not yet a cheap read) degrades to
// "waiting for first step…" rather than parsing a full transcript here.

import { useEffect, useMemo, useState } from "react";
import { phaseTone, type SquadOverviewData } from "@/components/SquadOverview";
import {
  recentRuns,
  recentTickets,
  selectProject,
  selectProjectAgents,
  type SquadAgentEntry,
  type SquadAgentsData,
} from "@/components/projects/ProjectLanding";
import { ApiError, listWorkItems } from "@/lib/tickets/api";
import { STATE_LABELS, type WorkItem } from "@/lib/tickets/types";
import {
  StatBand,
  StatTile,
  PanelCard,
  StackedAreaChart,
  HBarChart,
} from "@/components/overview";
import {
  areaXLabels,
  fetchProjectSeries,
  formatTokens,
  toAreaSeries,
  toRunBars,
  type SeriesState,
} from "@/lib/overview/projectSeries";
import "@/components/overview/overview.css";
import "@/components/overview/dashboard.css";

const THINKING_MAX = 128; // truncate the latest-thought snippet (design §3), full text in tooltip.
const LIVE_LIMIT = 6;
const TICKETS_LIMIT = 6;

// ---------------------------------------------------------------------------
// Data hooks — each an independent async boundary.
// ---------------------------------------------------------------------------

type OverviewState =
  | { kind: "loading" }
  | { kind: "error"; status: number }
  | { kind: "not-available" }
  | { kind: "ready"; data: SquadOverviewData };

function useSquadOverview(): OverviewState {
  const [state, setState] = useState<OverviewState>({ kind: "loading" });
  useEffect(() => {
    let alive = true;
    fetch("/api/squad/overview", { headers: { accept: "application/json" } })
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
        setState({ kind: "ready", data: (await res.json()) as SquadOverviewData });
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
  | { kind: "ready"; items: WorkItem[] };

function useWorkItems(projectId: string): WorkItemsState {
  const [state, setState] = useState<WorkItemsState>({ kind: "loading" });
  useEffect(() => {
    let alive = true;
    listWorkItems(projectId)
      .then((items) => alive && setState({ kind: "ready", items }))
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

/** S4 series with a manual retry token (the charts' PanelCard error state calls `retry`). */
function useProjectSeries(projectId: string): { state: SeriesState; retry: () => void } {
  const [state, setState] = useState<SeriesState>({ kind: "loading" });
  const [nonce, setNonce] = useState(0);
  useEffect(() => {
    const ac = new AbortController();
    setState({ kind: "loading" });
    fetchProjectSeries(projectId, "30d", ac.signal).then((s) => {
      if (!ac.signal.aborted) setState(s);
    });
    return () => ac.abort();
  }, [projectId, nonce]);
  return { state, retry: () => setNonce((n) => n + 1) };
}

// ---------------------------------------------------------------------------
// Small derivations.
// ---------------------------------------------------------------------------

const ACTIVE_RUN_PHASES = ["pending", "claiming", "dispatching", "running", "collecting"];

function activeRunCount(project: ReturnType<typeof selectProject>): number {
  return (project?.runs ?? []).filter((r) =>
    ACTIVE_RUN_PHASES.some((p) => r.phase.toLowerCase().includes(p)),
  ).length;
}

function truncateThought(text: string): string {
  return text.length > THINKING_MAX ? `${text.slice(0, THINKING_MAX)}…` : text;
}

// ---------------------------------------------------------------------------
// Component.
// ---------------------------------------------------------------------------

export function ProjectOverviewDashboard({ projectId }: { projectId: string }) {
  const overview = useSquadOverview();
  const workItems = useWorkItems(projectId);
  const agents = useSquadAgents();
  const { state: series, retry: retrySeries } = useProjectSeries(projectId);

  const project =
    overview.kind === "ready" ? selectProject(overview.data, projectId) : null;
  const namespace = project?.namespace;

  const roster =
    agents.kind === "ready" ? selectProjectAgents(agents.agents, namespace) : [];
  const items = workItems.kind === "ready" ? workItems.items : [];
  const openIssues = items.filter((it) => it.state !== "done").length;

  const dash = <span className="ov-tone--neutral">—</span>;
  const seriesReady = series.kind === "ready" ? series.series : null;
  const tokens = seriesReady?.tokens;

  // --- stat tiles ---------------------------------------------------------
  const tokensValue =
    series.kind === "loading"
      ? "…"
      : tokens?.available
        ? formatTokens(tokens.total)
        : dash;
  const issuesValue =
    workItems.kind === "loading"
      ? "…"
      : workItems.kind === "ready"
        ? openIssues
        : dash;
  const runsValue =
    overview.kind === "loading" ? "…" : project ? activeRunCount(project) : dash;
  const agentsValue =
    agents.kind === "loading" ? "…" : agents.kind === "ready" ? roster.length : dash;

  // --- charts -------------------------------------------------------------
  const ticketsAvail = seriesReady?.ticketsByStatus;
  const areaState = useMemo(() => {
    if (series.kind === "loading") return { loading: true, series: [], labels: [] as string[] };
    const snaps = ticketsAvail?.available ? ticketsAvail.snapshots ?? [] : [];
    return { loading: false, series: toAreaSeries(snaps), labels: areaXLabels(snaps) };
  }, [series.kind, ticketsAvail]);

  const runsAvail = seriesReady?.runsByStatus;
  const runBars = useMemo(
    () => (runsAvail?.available ? toRunBars(runsAvail.byPhase ?? {}) : []),
    [runsAvail],
  );

  // Chart panel state: distinguish the S4 degrade (not-available) from a retryable error.
  const chartPanelState =
    series.kind === "loading"
      ? ("loading" as const)
      : series.kind === "error"
        ? ("error" as const)
        : ("ready" as const);

  const liveRuns = recentRuns(project, LIVE_LIMIT);
  const latestTickets = recentTickets(items, TICKETS_LIMIT);
  const issuesHref = `/projects/${encodeURIComponent(projectId)}/issues`;
  const runsHref = `/projects/${encodeURIComponent(projectId)}/runs`;

  return (
    <div className="ov-dashboard" data-testid="project-overview-dashboard">
      {/* Stat band (5) — §3 */}
      <StatBand>
        <StatTile
          label="Tokens consumed"
          value={tokensValue}
          tone="accent"
          sub={tokens?.runsCounted != null ? `${tokens.runsCounted} runs · 30d` : "30d window"}
          loading={series.kind === "loading"}
        />
        {/* Open PRs has no project-scoped read model yet (design §4 GitHub mirror, ISI-4229) —
            degrade honestly rather than fabricate. */}
        <StatTile label="Open PRs" value={dash} tone="neutral" sub="not reported" />
        <StatTile
          label="Open issues"
          value={issuesValue}
          tone="neutral"
          loading={workItems.kind === "loading"}
        />
        <StatTile
          label="Active runs"
          value={runsValue}
          tone="running"
          loading={overview.kind === "loading"}
        />
        <StatTile
          label="Agents active"
          value={agentsValue}
          tone="run"
          loading={agents.kind === "loading"}
        />
      </StatBand>

      {/* Charts row — §3 (62% area / 38% h-bars) */}
      <div className="ov-charts-row">
        <PanelCard
          title="Tickets by status over time"
          state={
            chartPanelState === "ready" && !ticketsAvail?.available ? "empty" : chartPanelState
          }
          emptyLabel={
            ticketsAvail?.reason
              ? `Time-series unavailable — ${ticketsAvail.reason}`
              : "Time-series not available on this deployment yet."
          }
          onRetry={retrySeries}
        >
          <StackedAreaChart
            series={areaState.series}
            xLabels={areaState.labels}
            loading={areaState.loading}
            ariaLabel="Tickets by status over the last 30 days"
            caption="Backlog · In progress · In review · Done — last 30 days"
          />
        </PanelCard>

        <PanelCard
          title="Runs by status"
          state={
            chartPanelState === "ready" && !runsAvail?.available ? "empty" : chartPanelState
          }
          emptyLabel="Run status counts not available on this deployment yet."
          onRetry={retrySeries}
        >
          <HBarChart items={runBars} loading={series.kind === "loading"} ariaLabel="Runs by status" />
        </PanelCard>
      </div>

      {/* Live agent runs — latest thinking (full width) — §3 */}
      <PanelCard
        title="Live agent runs — latest thinking"
        action={{ label: "View all runs →", href: runsHref }}
        state={
          overview.kind === "loading"
            ? "loading"
            : overview.kind === "error"
              ? "error"
              : liveRuns.length === 0
                ? "empty"
                : "ready"
        }
        emptyLabel="No agent runs in this project yet."
      >
        <ul className="ov-thinking" data-testid="live-thinking">
          {liveRuns.map((r) => (
            <li key={r.name} className="ov-thinking__row">
              <div className="ov-thinking__head">
                <a href={`/runs/${encodeURIComponent(r.name)}${r.workItem ? `?wi=${encodeURIComponent(r.workItem)}` : ""}`}>
                  {r.name}
                </a>
                <span className="phase-chip" data-tone={phaseTone(r.phase)}>
                  {r.phase}
                </span>
              </div>
              {/* Latest-thought inset. A cheap per-run latest-step read is not wired yet
                  (design OQ2) — degrade to an honest "waiting" note, never a blank inset. */}
              <p className="ov-thinking__snippet" title="Latest step not yet available">
                {truncateThought("waiting for first step…")}
              </p>
            </li>
          ))}
        </ul>
      </PanelCard>

      {/* Bottom row — Latest tickets (55%) + Latest runs (45%) — §3 */}
      <div className="ov-bottom-row">
        <PanelCard
          title="Latest tickets updated"
          action={{ label: "View all → Issues", href: issuesHref }}
          state={
            workItems.kind === "loading"
              ? "loading"
              : workItems.kind === "not-available"
                ? "empty"
                : workItems.kind === "error"
                  ? "error"
                  : latestTickets.length === 0
                    ? "empty"
                    : "ready"
          }
          emptyLabel="No tickets yet."
        >
          <ul className="ov-list" data-testid="latest-tickets">
            {latestTickets.map((t) => (
              <li key={t.id} className="ov-list__row">
                <a href={`${issuesHref}?item=${encodeURIComponent(t.id)}`}>{t.title}</a>
                <span className="phase-chip" data-tone={t.state === "done" ? "idle" : "running"}>
                  {STATE_LABELS[t.state]}
                </span>
              </li>
            ))}
          </ul>
        </PanelCard>

        <PanelCard
          title="Latest agent runs"
          action={{ label: "View all runs →", href: runsHref }}
          state={
            overview.kind === "loading"
              ? "loading"
              : overview.kind === "error"
                ? "error"
                : liveRuns.length === 0
                  ? "empty"
                  : "ready"
          }
          emptyLabel="No runs yet."
        >
          <ul className="ov-list" data-testid="latest-runs">
            {liveRuns.map((r) => (
              <li key={r.name} className="ov-list__row">
                <a href={`/runs/${encodeURIComponent(r.name)}${r.workItem ? `?wi=${encodeURIComponent(r.workItem)}` : ""}`}>
                  {r.name}
                </a>
                <span className="phase-chip" data-tone={phaseTone(r.phase)}>
                  {r.phase}
                </span>
              </li>
            ))}
          </ul>
        </PanelCard>
      </div>
    </div>
  );
}

export default ProjectOverviewDashboard;
