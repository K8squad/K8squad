"use client";

// lib/tickets/liveIssueIds.ts — ISI-5528 (child of ISI-5520): the board-wide
// "which tickets have a live agent run right now" signal that feeds the list +
// kanban live-run markers.
//
// ONE source, reused — GET /api/squad/overview — the same per-project runs +
// phaseCounts projection the Overview surfaces already read (components/SquadOverview
// owns the wire shape + `phaseTone`, the run-hue authority). We do NOT invent a new
// endpoint or a new per-ticket poll: useActiveRunDiscovery (lib/tickets) is the
// per-open-ticket ambient bridge off GET /api/runs; THIS is the aggregate the board
// scans by. The join key is runs[].workItem === WorkItem.id (apiserver
// Run.Spec.WorkItemRef, overview.go).
//
// "Live" matches the task-list pill idiom (ISI-5520): a run is live when it is actively
// working (phaseTone collapses running/claiming/dispatching/collecting → "running") OR is
// still waiting to start (queued / WaitingForEndpointSlot). Queued is honest live presence
// — a just-minted run lights the marker before the agent claims it. Terminal
// (failed/canceled) and paused runs are NOT live.
//
// NOTE: the sibling S1 (ISI-5526, lib/overview/liveRuns.ts) carries a twin `isLiveRunPhase`
// for the left-nav badge. The two predicates are kept semantically identical so the shared
// derivation can be unified into one module once both children land (trivial follow-up) —
// neither PR depends on the other's unmerged file today.

import { useEffect, useState } from "react";
import { phaseTone, type SquadOverviewData } from "@/components/SquadOverview";
import { selectProject } from "@/components/projects/ProjectLanding";

/**
 * Poll cadence for the board live markers. Gentle — a presence cue, not a dispatch
 * ladder — and matches useActiveRunDiscovery's idle cadence. A backgrounded tab skips the
 * fetch (the user can't see the board); the bare timer keeps ticking and resumes on focus.
 */
export const LIVE_ISSUE_POLL_INTERVAL_MS = 5_000;

/** Does a run phase count as live presence for the board markers? (See file header.) */
export function isLiveRunPhase(phase: string): boolean {
  if (phaseTone(phase) === "running") return true;
  const p = phase.toLowerCase();
  return p.includes("queued") || p.includes("waiting");
}

/**
 * The set of work-item ids with ≥1 live run, sliced to one project. Pure (no fetch, no
 * DOM) so the gating rule is unit-tested without a network. Null-safe: `projects` and each
 * `runs` arrive nullable on the wire (Go nil slice → null). `projectId` matches the bare
 * name OR the "namespace/name" composite, exactly like {@link selectProject}.
 */
export function liveIssueIdsFromOverview(
  data: SquadOverviewData | null,
  projectId: string,
): Set<string> {
  const ids = new Set<string>();
  if (!data) return ids;
  const project = selectProject(data, projectId);
  for (const run of project?.runs ?? []) {
    if (run.workItem && isLiveRunPhase(run.phase)) ids.add(run.workItem);
  }
  return ids;
}

/** True when two id sets hold the same members — lets the poll keep a stable reference
 * across unchanged ticks, so the whole board doesn't re-render every 5s for no change. */
function sameIds(a: ReadonlySet<string>, b: ReadonlySet<string>): boolean {
  if (a.size !== b.size) return false;
  for (const id of a) if (!b.has(id)) return false;
  return true;
}

/**
 * Poll the shared overview feed and project it to the live-issue-id set for `projectId`.
 * Best-effort: a failed or not-yet-hosted (501/404) read leaves the set empty, so the board
 * simply shows no live markers — never a fabricated "live" state (FR-I3). Returns an empty
 * set until the first successful read.
 */
export function useLiveIssueIds(projectId: string): ReadonlySet<string> {
  const [ids, setIds] = useState<ReadonlySet<string>>(() => new Set<string>());

  useEffect(() => {
    if (!projectId || typeof window === "undefined") return;
    // Reset to empty when the project changes so a stale project's markers never bleed over.
    setIds((prev) => (prev.size === 0 ? prev : new Set<string>()));

    let cancelled = false;
    let timer: ReturnType<typeof setTimeout> | null = null;

    const schedule = () => {
      if (cancelled) return;
      timer = setTimeout(tick, LIVE_ISSUE_POLL_INTERVAL_MS);
    };

    const tick = async () => {
      if (typeof document !== "undefined" && document.visibilityState === "hidden") {
        schedule();
        return;
      }
      try {
        const res = await fetch("/api/squad/overview", {
          headers: { accept: "application/json" },
          cache: "no-store",
        });
        if (res.ok && !cancelled) {
          const data = (await res.json()) as SquadOverviewData;
          const next = liveIssueIdsFromOverview(data, projectId);
          setIds((prev) => (sameIds(prev, next) ? prev : next));
        }
      } catch {
        // best-effort presence cue — swallow and retry on the next tick
      }
      schedule();
    };

    tick();

    return () => {
      cancelled = true;
      if (timer) clearTimeout(timer);
    };
  }, [projectId]);

  return ids;
}
