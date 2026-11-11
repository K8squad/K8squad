"use client";

// RunEndedWatch — ISI-7273 (WS-3 of parent ISI-7262).
//
// The discussion room's ONE project SSE bus (lib/discussion/liveFeed.ts) carries
// room message changes and live `thinking` envelopes, but NO run-lifecycle
// milestones: the room only ever learns a dispatched run FINISHED through the
// final durable message arriving as a `message.created` frame. When that live
// push is missed, the transcript stalls until a manual refresh — the parent
// ISI-7262 symptom.
//
// This component mirrors the ticket view's ended → re-fetch pattern
// (lib/tickets/useTicketRunStream.ts fires onEnded on the run's terminal
// `ended` milestone; TicketDetail bumps its reload key; useThread re-fetches).
// The room learns a run's id ONLY from the `thinking` envelopes that run
// streams (postMessage never returns it — see the correlation note in
// lib/discussion/working.ts), so the room collects run ids off the bus and
// mounts ONE invisible watch per run. Each watch opens the SAME shared per-run
// SSE (useRunStream → /api/runs/{id}/stream, §13 "one bus, no polling").
// Attaching mid-run is safe: the browser's native EventSource auto-reconnect
// sends Last-Event-ID and the apiserver replays the durable coord-record tail
// (AC5), so the terminal `ended` milestone always (re)arrives. On the first
// `ended` the watch fires onEnded exactly once (guarded per mount, so a
// replayed milestone can never loop the re-fetch) and the parent drops the run
// id — unmounting the watch closes the stream.
//
// Degrade: a runtime without EventSource (jsdom, very old browsers) keeps the
// watch inert — the room then behaves exactly as before ISI-7273. The backstop
// is additive; there is still no second transport and no polling.

import { useEffect, useRef } from "react";
import { useRunStream } from "@/lib/useRunStream";

/**
 * True when a shared-stream LIFECYCLE event is the terminal `ended` milestone
 * (useRunStream stamps its summary as `"ended"` or `"ended: {step}"`). Same
 * predicate as the ticket view's private isEnded (useTicketRunStream.ts) —
 * duplicated here so the discussion domain stays import-clean of lib/tickets.
 */
export function isEndedMilestone(summary: string | undefined): boolean {
  return (summary ?? "").split(":")[0].trim() === "ended";
}

/**
 * Invisible per-run ended watch. Renders nothing; its only output is the
 * one-shot onEnded callback on the run's terminal milestone. The parent
 * unmounts the watch after the callback fires by dropping the tracked run id.
 */
export function RunEndedWatch({
  runId,
  onEnded,
}: {
  runId: string;
  onEnded: (runId: string) => void;
}) {
  // No EventSource ⇒ inert (see header). Passing "" keeps useRunStream from
  // ever constructing the stream in that environment.
  const canStream =
    typeof window !== "undefined" && typeof window.EventSource === "function";
  const { events } = useRunStream(canStream ? runId : "");

  // Fire once per mount: a replayed `ended` (native SSE reconnect /
  // Last-Event-ID replay) must never loop the parent's re-fetch. Mirrors the
  // ticket hook's firedFor guard (useTicketRunStream.ts).
  const fired = useRef(false);
  const onEndedRef = useRef(onEnded);
  onEndedRef.current = onEnded;
  useEffect(() => {
    if (fired.current) return;
    if (
      events.some((e) => e.kind === "LIFECYCLE" && isEndedMilestone(e.summary))
    ) {
      fired.current = true;
      onEndedRef.current(runId);
    }
  }, [events, runId]);

  return null;
}
