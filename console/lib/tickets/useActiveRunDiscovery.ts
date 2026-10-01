"use client";

// lib/tickets/useActiveRunDiscovery.ts — ISI-5287 (child of ISI-5285).
//
// The "run started AFTER the tab was already open" gap.
//
// ISI-5248 fixed "connect AFTER the run exists": a FRESH tab opened on a ticket
// resolves the run NAME→UID and replays the durable tail, so it shows the agent's
// output. ISI-5206 (useTicketRunStream) live-tails the ticket's CURRENT holding run
// off the shared per-run SSE bus. BUT both of those key entirely off the thread read
// model (thread.runId / thread.state), which is only re-fetched when the user posts a
// comment, or when a run this tab is ALREADY streaming ends (onRunSettled). So a tab
// that is sitting open with NO live run — and then a run starts (Intake mints it
// seconds after a comment; another tab/admin/session dispatches; a discussion
// @-mention fires; kanban automation runs) — never learns a new run exists: its
// thread.runId stays empty/stale, ambientRunId() returns "", no EventSource ever
// opens, and the "working" indicator never clears. That is Henrik's exact repro
// (ISI-5285): leave the tab open, comment, john starts, the tab is stuck on "john is
// working" forever while a fresh tab shows the reply.
//
// This hook closes that gap with the SMALLEST possible mechanism: while the tab
// believes NOTHING is live (no self-dispatch ladder stream, no ambient stream — i.e.
// exactly the idle window the bug lives in), it polls the existing `GET /api/runs`
// listing — the SAME discovery source useDispatchWatch already uses (§13 "one bus";
// no new backend, schema, or token) — for a live run on this work item. The instant
// one appears whose id the tab is not already tracking, it fires `onRunAppeared` to
// re-fetch the thread. That reload populates thread.runId + flips thread.state into a
// running lane, which engages the EXISTING ambient useTicketRunStream — whose
// fresh-connect backfill (ISI-5248) then replays anything emitted before the connect
// and live-tails the rest. Once a stream is open (`streaming` true) this hook goes
// inert and the SSE bus + its terminal onEnded reload own the lifecycle; when that run
// ends and the tab returns to idle, the hook re-engages to catch the NEXT run.

import { useEffect, useRef } from "react";

/** The subset of a `GET /api/runs` row (apiserver RunListItem) this bridge reads. */
interface RunListRow {
  id?: string;
  phase?: string;
  workItemRef?: string;
}

/**
 * Discovery poll cadence for the idle open tab. Gentler than useDispatchWatch's 2s
 * active-dispatch poll — this is a passive safety net that runs only while nothing is
 * streaming, not a tight ladder bridge. No hard cap: the user may leave a ticket open
 * for a long time waiting for the next run, and a cap would silently re-open the very
 * gap this hook exists to close. Backgrounded tabs skip the fetch (see below), so an
 * idle loop is a bare setTimeout, not a network cost.
 */
export const ACTIVE_RUN_POLL_INTERVAL_MS = 5_000;

/**
 * Run phases where the run holds the work item and events actively flow — so a thread
 * re-fetch is guaranteed to populate thread.runId (the holder is set) and move the
 * ticket into a running lane, which is what engages the ambient stream. Pending is
 * deliberately excluded: a not-yet-claimed run has no holder, so reloading on it would
 * not open the stream (and the fresh-connect backfill replays its early events once we
 * do catch it at Claiming/Running).
 */
const LIVE_PHASES: ReadonlySet<string> = new Set(["Claiming", "Running"]);

/**
 * Pick the live run row for this work item out of a `GET /api/runs` array — the newest
 * one in a holding phase (LIVE_PHASES). Pure, so the gating rule is unit-tested without
 * a network. Order-stable: the listing is newest-first server-side, so the first live
 * match is the most recent run; we take it rather than re-sorting (the listing carries
 * no reliable per-row start timestamp for in-flight runs, see pickDispatchRun).
 */
export function pickActiveRun(
  rows: RunListRow[],
  workItemId: string,
): RunListRow | null {
  for (const r of rows) {
    if (r.workItemRef !== workItemId) continue;
    if (typeof r.id !== "string" || !r.id) continue;
    if (!r.phase || !LIVE_PHASES.has(r.phase)) continue;
    return r;
  }
  return null;
}

/**
 * Detect a newly-started run on an already-open ticket tab and trigger a thread
 * re-fetch so the ambient SSE stream (useTicketRunStream) engages (ISI-5287).
 *
 * @param workItemId   the ticket id ("" ⇒ inert)
 * @param holdingRunId the run the tab already knows (thread.runId) — never re-discovered
 * @param streaming    true when a live stream is already open (self-dispatch ladder or
 *                     ambient) ⇒ the hook stays inert and lets the SSE bus own the run
 * @param onRunAppeared fired once per newly-discovered run id — re-fetch the thread
 */
export function useActiveRunDiscovery(
  workItemId: string,
  holdingRunId: string,
  streaming: boolean,
  onRunAppeared: () => void,
): void {
  // Keep the latest callback without re-arming the poll loop every render.
  const onRef = useRef(onRunAppeared);
  onRef.current = onRunAppeared;
  // The run id we last triggered a reload for — a hard backstop so a live run we have
  // already surfaced can never loop the thread re-fetch even if the reload is slow to
  // reflect it (the holding-phase gate already guarantees convergence in one reload;
  // this guards the pathological read-model-lag case).
  const actedFor = useRef<string>("");

  useEffect(() => {
    // Inert while a stream is already open (the SSE bus + its onEnded reload own the
    // lifecycle) or with no ticket. We poll ONLY in the idle gap — the tab believes
    // nothing is live — which is precisely the "run started after the tab opened"
    // window this hook exists to cover.
    if (!workItemId || streaming) return;
    if (typeof window === "undefined") return;

    let cancelled = false;
    let timer: ReturnType<typeof setTimeout> | null = null;

    const schedule = () => {
      if (cancelled) return;
      timer = setTimeout(tick, ACTIVE_RUN_POLL_INTERVAL_MS);
    };

    const tick = async () => {
      // Don't poll a backgrounded tab — the user can't see it. The loop keeps ticking
      // (cheap bare timer) and resumes fetching the moment the tab is foregrounded, so
      // no visibilitychange listener is needed.
      if (typeof document !== "undefined" && document.visibilityState === "hidden") {
        schedule();
        return;
      }
      try {
        const res = await fetch("/api/runs", {
          headers: { accept: "application/json" },
          cache: "no-store",
        });
        if (res.ok) {
          const body = (await res.json()) as unknown;
          const rows = Array.isArray(body) ? (body as RunListRow[]) : [];
          const row = pickActiveRun(rows, workItemId);
          if (
            row &&
            row.id &&
            !cancelled &&
            row.id !== holdingRunId &&
            row.id !== actedFor.current
          ) {
            // A live run the tab isn't already tracking → reload the thread so
            // thread.runId/state update and the ambient SSE opens. The reload flips
            // `streaming` true, which tears down this effect (one reload, no loop).
            actedFor.current = row.id;
            onRef.current();
            return;
          }
        }
      } catch {
        // best-effort bridge — swallow and retry on the next tick
      }
      schedule();
    };

    tick();

    return () => {
      cancelled = true;
      if (timer) clearTimeout(timer);
    };
  }, [workItemId, holdingRunId, streaming]);
}
