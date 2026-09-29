"use client";

// lib/tickets/useTicketRunStream.ts — ISI-5206 (S3 of ISI-5202).
//
// Problem 2 of ISI-5202: a human posts a message on a ticket, a run starts, but
// the Activity feed spins and never streams — the user has to refresh to see the
// agent's output. Root cause (S1 / ISI-5204 verdict): the ticket only live-tails
// a run it DISPATCHED itself this browser session — useDispatchWatch arms the
// shared per-run SSE (useRunStream) only for a self-dispatch, so a run started any
// OTHER way (the kanban automation, another admin/session, a discussion @-mention,
// or simply the page opened while a run is already in flight) has no dispatchWatch
// and its per-run `thinking` SSE is never opened. Nothing appends until a manual
// reload materializes the durable coord.comment rows.
//
// This hook closes that gap. It opens the SAME shared per-run SSE bus
// (useRunStream → /api/runs/{id}/stream, §13 "one bus, no polling") for the
// ticket's CURRENT holding run (thread.runId) whenever that run is live — using
// the SAME `isRunningState` predicate the run-meta "running" dot already trusts,
// so we stream precisely when the UI already claims the run is in flight,
// regardless of who started it. It returns the live `thinking` rows reduced to the
// LiveThinkingRow triple the Activity feed renders (mergeLiveThinking de-dups them
// against the durable comments on reload), and — on the run's terminal `ended`
// milestone — fires `onEnded` so the caller re-fetches the thread. That single
// re-fetch materializes the final durable comments AND flips the ticket out of its
// running state, so the live surface always resolves without a manual refresh
// (the "terminal state so the spinner resolves" ask).
//
// ONE EventSource per run (ISI-5174 AC4): when the self-dispatch ladder is already
// streaming this very run (dispatchWatch.runId === thread.runId), this hook stays
// inert so the two never double-open the same stream.

import { useEffect, useMemo, useRef } from "react";
import { useRunStream } from "@/lib/useRunStream";
import { isRunningState } from "@/lib/tickets/runComments";
import type { LiveThinkingRow } from "@/lib/tickets/thread";

/**
 * The run id whose SSE stream the ticket should ambiently tail — the current
 * holding run when it is live and NOT already covered by the self-dispatch ladder.
 * Empty ⇒ nothing to open (no run, a terminal/intake lane, or the self-dispatch
 * hook already owns this run's stream). Pure so the gating rule is unit-tested
 * without a stream.
 */
export function ambientRunId(
  runId: string,
  state: string,
  dispatchRunId: string | undefined,
): string {
  if (!runId) return ""; // no holding run
  if (!isRunningState(state)) return ""; // terminal/intake — nothing live to tail
  if (dispatchRunId && dispatchRunId === runId) return ""; // self-dispatch already streaming
  return runId;
}

/** True when a shared-stream LIFECYCLE event is the terminal `ended` milestone
 * (useRunStream stamps its summary as `"ended"` or `"ended: {step}"`). */
function isEnded(summary: string | undefined): boolean {
  return (summary ?? "").split(":")[0].trim() === "ended";
}

/**
 * Live-tail the ticket's active holding run (see ambientRunId) off the shared
 * per-run SSE bus. Returns the live `thinking` rows in arrival order, and invokes
 * `onEnded` exactly once when the run reports its terminal milestone so the caller
 * can re-fetch the durable thread. Inert (returns []) whenever there is no ambient
 * run to open.
 *
 * @param runId         the ticket's current holding run (thread.runId; "" ⇒ none)
 * @param state         the ticket lane (drives the `isRunningState` liveness gate)
 * @param dispatchRunId the run the self-dispatch ladder is already streaming, if any
 * @param onEnded       fired once on the run's `ended` milestone (re-fetch the thread)
 */
export function useTicketRunStream(
  runId: string,
  state: string,
  dispatchRunId: string | undefined,
  onEnded: () => void,
): LiveThinkingRow[] {
  const id = ambientRunId(runId, state, dispatchRunId);
  const { events } = useRunStream(id);

  // Re-fetch the thread the instant the run ends — materialize the durable tail
  // and flip the running state so the feed resolves without a manual refresh.
  // Guarded per-stream-id so a replayed `ended` (SSE reconnect / a re-fetch that
  // keeps the ticket in a Review lane) can never loop the re-fetch.
  const firedFor = useRef<string>("");
  const onEndedRef = useRef(onEnded);
  onEndedRef.current = onEnded;
  useEffect(() => {
    if (!id) {
      firedFor.current = "";
      return;
    }
    if (firedFor.current === id) return;
    if (events.some((e) => e.kind === "LIFECYCLE" && isEnded(e.summary))) {
      firedFor.current = id;
      onEndedRef.current();
    }
  }, [id, events]);

  return useMemo(
    () =>
      id
        ? events
            .filter((e) => e.kind === "THINKING")
            .map((e) => ({ author: e.actor, body: e.summary ?? "", at: e.ts }))
        : [],
    [id, events],
  );
}
