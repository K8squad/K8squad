"use client";

// components/runs/RunsList.tsx — the ISI-4575 runs list screen body, shared by both mounts:
//   • /runs                      (cross-project / fleet view)
//   • /projects/{id}/runs        (project-scoped view, `projectId` prop set)
//
// Built to the approved ISI-4569 mock (runs-list-mock.html): filter bar (phase / agent /
// time window), a run table with phase chips, and offset pagination. Reads the ISI-4571
// read models through the BFF choke point (never the apiserver directly):
//   • GET /api/runs?phase=&agent=&window=&limit=&offset=            (fleet)
//   • GET /api/projects/{id}/runs?…                                 (project-scoped)
//
// Fabrication discipline (FR-I3): a field the read model does not surface (agent for
// reconciler-defaulted runs, tokens when the run-drive has reported none) renders "—";
// only authoritative values render real data. Degrade-don't-blank (ISI-4229): 404/501
// (endpoint unwired on this deployment) renders an honest not-available card, other
// failures a retryable error card — never a blank page.

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useRouter } from "next/navigation";
import { phaseTone } from "@/components/SquadOverview";
import { EmptyState } from "@/components/forms/EmptyState";
import { formatTokens } from "@/lib/overview/projectSeries";
import "./runs.css";

/** GET /api/runs item (apiserver RunListItem, internal/apiserver/runs.go). */
export interface RunListItem {
  id: string;
  name: string;
  phase: string;
  pausedReason?: string;
  workItemRef: string;
  projectRef: string;
  agents?: string[] | null;
  totalTokens?: number | null;
  startedAt?: string | null;
  endedAt?: string | null;
  durationSeconds?: number | null;
  traceId?: string;
}

export const RUNS_PAGE_SIZE = 25;

/** The Run CRD phases (api/v1alpha1/run_types.go) offered as filter values. */
export const RUN_PHASES = [
  "Pending",
  "Claiming",
  "Running",
  "Paused",
  "Canceling",
  "Succeeded",
  "Failed",
  "Cancelled",
] as const;

/** Time-window filter values the apiserver understands (runInTimeWindow). */
const WINDOWS = [
  { value: "", label: "All time" },
  { value: "1h", label: "Last hour" },
  { value: "24h", label: "Last 24 hours" },
  { value: "7d", label: "Last 7 days" },
] as const;

type LoadState =
  | { kind: "loading" }
  | { kind: "error"; status: number }
  | { kind: "not-available" }
  | { kind: "ready"; runs: RunListItem[] };

/** startedAt arrives as the Go zero time ("0001-01-01…") when the run was never claimed —
 * treat anything before 2000 as absent. */
function validDate(iso: string | null | undefined): Date | null {
  if (!iso) return null;
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) || d.getFullYear() < 2000 ? null : d;
}

export function formatAge(iso: string | null | undefined): string {
  const d = validDate(iso);
  if (!d) return "—";
  const s = Math.max(0, Math.floor((Date.now() - d.getTime()) / 1000));
  if (s < 60) return `${s}s ago`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ago`;
  const h = Math.floor(m / 60);
  if (h < 48) return `${h}h ago`;
  return `${Math.floor(h / 24)}d ago`;
}

export function formatDuration(seconds: number | null | undefined): string {
  if (seconds == null || !Number.isFinite(seconds) || seconds <= 0) return "—";
  const s = Math.floor(seconds);
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ${s % 60}s`;
  const h = Math.floor(m / 60);
  return `${h}h ${m % 60}m`;
}

/** Deep link into the run detail screen; the work-item key rides `?wi=` exactly like the
 * overview run links (RunStream/KillRun resolve the coord claim from it). */
export function runHref(run: RunListItem): string {
  const base = `/runs/${encodeURIComponent(run.name)}`;
  return run.workItemRef ? `${base}?wi=${encodeURIComponent(run.workItemRef)}` : base;
}

export function RunsList({
  projectId,
  projectName,
}: {
  /** Canonical project id (ns/name or name) — when set, the list is project-scoped and the
   * Project column/filter are hidden. */
  projectId?: string;
  /** Display name for the scoped heading. */
  projectName?: string;
}) {
  const router = useRouter();
  const [phase, setPhase] = useState("");
  const [agentInput, setAgentInput] = useState("");
  const [agent, setAgent] = useState("");
  const [window_, setWindow] = useState("");
  const [offset, setOffset] = useState(0);
  const [tick, setTick] = useState(0); // retry retrigger
  const [state, setState] = useState<LoadState>({ kind: "loading" });

  // Debounce the free-text agent filter so typing doesn't fire a request per keystroke.
  const debounceRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const onAgentChange = useCallback((value: string) => {
    setAgentInput(value);
    if (debounceRef.current) clearTimeout(debounceRef.current);
    debounceRef.current = setTimeout(() => setAgent(value.trim()), 300);
  }, []);
  useEffect(
    () => () => {
      if (debounceRef.current) clearTimeout(debounceRef.current);
    },
    [],
  );

  // Any filter change restarts pagination from the first page.
  useEffect(() => {
    setOffset(0);
  }, [phase, agent, window_, projectId]);

  const endpoint = useMemo(
    () =>
      projectId
        ? `/api/projects/${encodeURIComponent(projectId)}/runs`
        : "/api/runs",
    [projectId],
  );

  useEffect(() => {
    let alive = true;
    setState({ kind: "loading" });
    const params = new URLSearchParams();
    if (phase) params.set("phase", phase);
    if (agent) params.set("agent", agent);
    if (window_) params.set("window", window_);
    params.set("limit", String(RUNS_PAGE_SIZE));
    params.set("offset", String(offset));
    fetch(`${endpoint}?${params.toString()}`, {
      headers: { accept: "application/json" },
    })
      .then(async (res) => {
        if (!alive) return;
        if (!res.ok) {
          setState(
            res.status === 404 || res.status === 501
              ? { kind: "not-available" }
              : { kind: "error", status: res.status },
          );
          return;
        }
        const body = (await res.json()) as RunListItem[] | null;
        setState({ kind: "ready", runs: body ?? [] });
      })
      .catch(() => alive && setState({ kind: "error", status: 0 }));
    return () => {
      alive = false;
    };
  }, [endpoint, phase, agent, window_, offset, tick]);

  const runs = state.kind === "ready" ? state.runs : [];
  const hasFilters = phase !== "" || agentInput !== "" || window_ !== "";
  const clearFilters = useCallback(() => {
    setPhase("");
    setWindow("");
    setAgentInput("");
    setAgent("");
  }, []);

  return (
    <section className="runs-screen" data-testid="runs-screen">
      <header className="runs-head">
        <h1>{projectId ? `Runs — ${projectName ?? projectId}` : "Runs"}</h1>
        {state.kind === "ready" && (
          <span className="muted runs-head__count" data-testid="runs-count">
            {offset + 1}–{offset + runs.length} shown
          </span>
        )}
      </header>

      <div className="runs-filters" data-testid="runs-filters">
        <div className="runs-filters__head">
          <span className="runs-filters__title">Filters</span>
          {hasFilters && (
            <button
              type="button"
              className="runs-filters__clear"
              onClick={clearFilters}
              data-testid="runs-clear-filters"
            >
              Clear all
            </button>
          )}
        </div>
        <div className="runs-filters__grid">
          <label className="runs-filter">
            <span className="runs-filter__label">Phase</span>
            <select
              className="runs-filter__select"
              value={phase}
              onChange={(e) => setPhase(e.target.value)}
              data-testid="runs-filter-phase"
            >
              <option value="">All phases</option>
              {RUN_PHASES.map((p) => (
                <option key={p} value={p}>
                  {p}
                </option>
              ))}
            </select>
          </label>
          <label className="runs-filter">
            <span className="runs-filter__label">Agent</span>
            <input
              className="runs-filter__select"
              type="text"
              placeholder="All agents"
              value={agentInput}
              onChange={(e) => onAgentChange(e.target.value)}
              data-testid="runs-filter-agent"
            />
          </label>
          <label className="runs-filter">
            <span className="runs-filter__label">Time window</span>
            <select
              className="runs-filter__select"
              value={window_}
              onChange={(e) => setWindow(e.target.value)}
              data-testid="runs-filter-window"
            >
              {WINDOWS.map((w) => (
                <option key={w.value} value={w.value}>
                  {w.label}
                </option>
              ))}
            </select>
          </label>
        </div>
      </div>

      {state.kind === "loading" && (
        <div className="card runs-state" data-testid="runs-loading">
          <p className="muted">Loading runs…</p>
        </div>
      )}

      {state.kind === "not-available" && (
        <EmptyState
          testId="runs-not-available"
          title="Run history is not available on this deployment"
          why="The run-list read model is not wired yet. Live runs remain visible on the Overview dashboards."
        />
      )}

      {state.kind === "error" && (
        <div className="card runs-state" data-testid="runs-error">
          <p>
            Could not load runs{state.status ? ` (HTTP ${state.status})` : ""}.
          </p>
          <button
            type="button"
            className="btn btn--primary"
            onClick={() => setTick((t) => t + 1)}
          >
            Retry
          </button>
        </div>
      )}

      {state.kind === "ready" && runs.length === 0 && (
        <EmptyState
          testId="runs-empty"
          title="No runs found"
          why={
            hasFilters
              ? "No runs match the current filters. Try widening the time window or clearing the filters."
              : "No runs have been triggered yet. Runs appear here as soon as the squad starts working."
          }
          ctaLabel={hasFilters ? "Clear filters" : undefined}
          onCta={hasFilters ? clearFilters : undefined}
        />
      )}

      {state.kind === "ready" && runs.length > 0 && (
        <div className="runs-table-wrap" data-testid="runs-table">
          <table className="runs-table">
            <thead>
              <tr>
                <th>Phase</th>
                <th>Run</th>
                <th>Agent</th>
                {!projectId && <th>Project</th>}
                <th>Started</th>
                <th>Duration</th>
                <th>Tokens</th>
              </tr>
            </thead>
            <tbody>
              {runs.map((r) => (
                <tr
                  key={r.id}
                  className="runs-row"
                  data-testid={`runs-row-${r.name}`}
                  onClick={() => router.push(runHref(r))}
                >
                  <td>
                    <span className="phase-chip" data-tone={phaseTone(r.phase)}>
                      {r.phase || "Unknown"}
                    </span>
                    {r.pausedReason && (
                      <div className="muted runs-row__sub">{r.pausedReason}</div>
                    )}
                  </td>
                  <td>
                    <a
                      href={runHref(r)}
                      className="runs-link"
                      onClick={(e) => e.stopPropagation()}
                      data-testid={`runs-link-${r.name}`}
                    >
                      {r.name}
                    </a>
                    {r.workItemRef && (
                      <div className="muted runs-row__sub" title={r.workItemRef}>
                        wi {r.workItemRef.slice(0, 8)}
                      </div>
                    )}
                  </td>
                  <td className="runs-cell-dim">
                    {r.agents && r.agents.length > 0 ? r.agents.join(", ") : "—"}
                  </td>
                  {!projectId && (
                    <td>
                      {r.projectRef ? (
                        <a
                          href={`/projects/${encodeURIComponent(r.projectRef)}`}
                          className="runs-link"
                          onClick={(e) => e.stopPropagation()}
                        >
                          {r.projectRef}
                        </a>
                      ) : (
                        <span className="runs-cell-dim">—</span>
                      )}
                    </td>
                  )}
                  <td className="runs-cell-dim">{formatAge(r.startedAt)}</td>
                  <td className="runs-cell-dim">
                    {formatDuration(r.durationSeconds)}
                  </td>
                  <td>
                    {r.totalTokens != null ? (
                      <span className="runs-tokens">
                        {formatTokens(r.totalTokens)}
                      </span>
                    ) : (
                      <span className="runs-cell-dim">—</span>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>

          <div className="runs-pagination" data-testid="runs-pagination">
            <span className="muted">
              Page {Math.floor(offset / RUNS_PAGE_SIZE) + 1}
            </span>
            <div className="runs-pagination__controls">
              <button
                type="button"
                className="runs-pagination__btn"
                disabled={offset === 0}
                onClick={() => setOffset(Math.max(0, offset - RUNS_PAGE_SIZE))}
                data-testid="runs-prev"
              >
                Previous
              </button>
              <button
                type="button"
                className="runs-pagination__btn"
                disabled={runs.length < RUNS_PAGE_SIZE}
                onClick={() => setOffset(offset + RUNS_PAGE_SIZE)}
                data-testid="runs-next"
              >
                Next
              </button>
            </div>
          </div>
        </div>
      )}
    </section>
  );
}
