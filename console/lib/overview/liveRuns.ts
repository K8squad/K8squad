// lib/overview/liveRuns.ts — ISI-5526 (parent ISI-5520): per-project live-run presence,
// derived from the ONE live-runs source this console has — GET /api/squad/overview.
//
// This is a thin projection over the SAME wire shape the Overview surfaces already read
// (SquadOverviewData, components/SquadOverview). No new endpoint, no new schema: the nav badge
// counts the live runs the overview feed already exposes. phaseTone (the console's run-hue
// authority) is reused verbatim as the "running" test; queued/waiting/pending phases read as idle
// there, so we add them here to match the task-list live idiom, where queued already counts as live.

import {
  phaseTone,
  type SquadOverviewData,
} from "@/components/SquadOverview";

/**
 * Does a run phase count as a live-activity cue? True when the run is actively working
 * (phaseTone collapses running/claiming/dispatching/collecting → "running") OR is waiting to
 * start — the Run state machine's `Pending` (the just-created/unclaimed phase overview coalesces
 * an empty status to), plus `Queued` / `WaitingForEndpointSlot`. This mirrors the task-list pill's
 * `status ∈ {queued, running}` semantics — a queued run is honest live presence, not idle. The
 * overview lists every Run in the namespace (terminal ones included), so terminal
 * (succeeded/failed/canceled) and paused phases MUST read as NOT live here.
 */
export function isLiveRunPhase(phase: string): boolean {
  if (phaseTone(phase) === "running") return true;
  const p = phase.toLowerCase();
  return p.includes("pending") || p.includes("queued") || p.includes("waiting");
}

/** The id the /api/projects BFF + nav tree key a project by: `namespace/name` (or the bare name
 * when the wire carries no namespace). Mirrored here so every per-project fold joins on the SAME
 * key, which also disambiguates same-named projects across squads in fleet mode. */
function projectKey(p: { name: string; namespace?: string }): string {
  return p.namespace ? `${p.namespace}/${p.name}` : p.name;
}

/**
 * Per-project count of live runs, keyed to match the nav tree's project id. The /api/projects BFF
 * derives that id from the SAME overview wire as `namespace ? "namespace/name" : name`
 * (app/api/projects/route.ts) — so we mirror that join key exactly, which also disambiguates
 * same-named projects across squads in fleet mode. Projects with zero live runs are omitted, so a
 * caller reads a count with `map[id] ?? 0` and renders nothing for idle rooms. Null-safe:
 * `projects` and each `runs` arrive nullable on the wire (Go nil slice → null).
 */
export function liveRunCountByProject(
  data: SquadOverviewData | null,
): Record<string, number> {
  const out: Record<string, number> = {};
  for (const p of data?.projects ?? []) {
    let n = 0;
    for (const r of p.runs ?? []) {
      if (isLiveRunPhase(r.phase)) n += 1;
    }
    if (n > 0) out[projectKey(p)] = n;
  }
  return out;
}

// ---------------------------------------------------------------------------
// Per-agent live-run presence (ISI-5527, parent ISI-5520). The discussion-room
// roster flips a row idle⇄running from the SAME overview feed the nav badge polls
// (no new endpoint, no new poll). A live run attributes to an agent via the
// apiserver's run.Spec.Agents (RunStatus.agents) — the same attribution org.go
// folds into the agent's four-value status; surfacing it on the run lets the
// roster paint the honest ladder idle → queued → running without a second read.
// ---------------------------------------------------------------------------

/** One agent's live-run presence, derived from the project's overview runs. */
export interface AgentRunPresence {
  /** `running` once the run is actively working (phaseTone "running"); `queued` while it waits to
   * start (Pending / Queued / WaitingForEndpointSlot) — queued already counts as live (OQ4). */
  state: "queued" | "running";
  /** The raw run phase, kept for legibility/testing (not rendered verbatim). */
  phase: string;
  /** The run's work item (ticket ref) for the roster sub-label, when the run carries one. */
  workItem?: string;
}

/** Is this live phase actively running (vs merely queued/waiting)? */
function isRunningPhase(phase: string): boolean {
  return phaseTone(phase) === "running";
}

/**
 * Per-agent live-run presence for ONE project, keyed by the dispatched agent NAME (the roster's
 * `id`/`name`; a roster is namespace-scoped so names are unique). `projectId` is the canonical
 * `namespace/name` the discussion room is routed by (lib/projectId.ts) — matched against the SAME
 * projectKey() the nav badge joins on, so the roster and the nav read the identical project. Only
 * live runs (isLiveRunPhase) that name an agent (run.agents) contribute — an agent-less
 * (Team-fanned) run is never mis-attributed to one agent (FR-I3: no fabricated per-agent state).
 * When an agent holds more than one live run, the actively-running one wins over a merely-queued
 * one so the row reflects the strongest honest signal. Agents with no live run are absent → the
 * roster renders them idle. Null-safe for the nullable wire shape.
 */
export function liveRunsByAgent(
  data: SquadOverviewData | null,
  projectId: string,
): Record<string, AgentRunPresence> {
  const out: Record<string, AgentRunPresence> = {};
  const project = (data?.projects ?? []).find(
    (p) => projectKey(p) === projectId || p.name === projectId,
  );
  if (!project) return out;
  for (const r of project.runs ?? []) {
    if (!isLiveRunPhase(r.phase)) continue;
    const running = isRunningPhase(r.phase);
    for (const agent of r.agents ?? []) {
      const prev = out[agent];
      // Running beats queued; first-seen otherwise. A later queued run never downgrades a row the
      // console already knows is running.
      if (prev && (prev.state === "running" || !running)) continue;
      out[agent] = {
        state: running ? "running" : "queued",
        phase: r.phase,
        workItem: r.workItem || undefined,
      };
    }
  }
  return out;
}
