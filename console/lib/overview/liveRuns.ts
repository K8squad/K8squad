// lib/overview/liveRuns.ts — ISI-5526 (parent ISI-5520): per-project live-run presence,
// derived from the ONE live-runs source this console has — GET /api/squad/overview.
//
// This is a thin projection over the SAME wire shape the Overview surfaces already read
// (SquadOverviewData, components/SquadOverview). No new endpoint, no new schema: the nav badge
// counts the live runs the overview feed already exposes. phaseTone (the console's run-hue
// authority) is reused verbatim as the "running" test; queued/waiting phases read as idle there,
// so we add them here to match the task-list live idiom, where queued already counts as live.

import {
  phaseTone,
  type SquadOverviewData,
} from "@/components/SquadOverview";

/**
 * Does a run phase count as a live-activity cue? True when the run is actively working
 * (phaseTone collapses running/claiming/dispatching/collecting → "running") OR is waiting to
 * start (queued / WaitingForEndpointSlot). This mirrors the task-list pill's `status ∈ {queued,
 * running}` semantics — queued is honest live presence, not idle. Terminal (failed/canceled) and
 * paused phases are NOT live.
 */
export function isLiveRunPhase(phase: string): boolean {
  if (phaseTone(phase) === "running") return true;
  const p = phase.toLowerCase();
  return p.includes("queued") || p.includes("waiting");
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
    if (n > 0) {
      const id = p.namespace ? `${p.namespace}/${p.name}` : p.name;
      out[id] = n;
    }
  }
  return out;
}
