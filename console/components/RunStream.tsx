"use client";

// components/RunStream.tsx — the live Run-progress timeline (story 8.2).
//
// Consumes the ONE shared EventSource (lib/useRunStream) and renders the coordination-event
// timeline: kind badge + actor·role + mono timestamp. READ-ONLY legibility — no claim/mutate/
// transition control rides the feed (AC6). Kill Run (FR-F4) is a separate control-plane action
// owned by story 3.3/8.4, deliberately NOT a stream verb, so it is absent here.

import { useEffect, useRef } from "react";

import { useRunStream, type RunEventKind } from "@/lib/useRunStream";

const KIND_LABEL: Record<RunEventKind, string> = {
  CHECKOUT: "checkout",
  COMMENT: "comment",
  HANDOFF: "handoff",
  MEMORY: "memory",
  ARTIFACT: "artifact",
  STEP: "step",
  LIFECYCLE: "lifecycle",
  THINKING: "thinking",
};

export function RunStream({
  runId,
  onMilestone,
}: {
  runId: string;
  /**
   * Fired once per newly-arrived lifecycle milestone (assigned/scheduled/
   * sandbox_bound/started/ended). RunDetail uses this to refetch its otherwise
   * fetch-once snapshot so the phase chip, lifecycle rail and token/tool summary
   * stop going stale — G1 (ISI-5457). Rides THIS component's single EventSource;
   * no second stream is opened. Deduped on the outbox event id (the SSE `id:`
   * line) so a reconnect replay re-delivering earlier milestones cannot
   * re-trigger a refetch — the same guard the ticket surface uses.
   */
  onMilestone?: () => void;
}) {
  const { events, status } = useRunStream(runId);

  const seen = useRef<Set<string>>(new Set());
  const onMilestoneRef = useRef(onMilestone);
  onMilestoneRef.current = onMilestone;
  useEffect(() => {
    for (const e of events) {
      if (e.kind !== "LIFECYCLE") continue;
      const key = e.id || `${e.summary ?? ""}@${e.ts}`;
      if (seen.current.has(key)) continue;
      seen.current.add(key);
      onMilestoneRef.current?.();
    }
  }, [events]);

  return (
    <section
      className="run-stream"
      aria-label={`Live progress for run ${runId}`}
    >
      <header className="run-stream__head">
        <span className={`stream-status stream-status--${status}`}>
          {status === "open" ? "live" : status}
        </span>
        <span className="run-stream__source">
          via coordination record (work items · comments · artifacts)
        </span>
      </header>

      {events.length === 0 ? (
        <p className="run-stream__empty">
          No events yet — waiting for the run to emit progress…
        </p>
      ) : (
        <ol className="run-stream__timeline">
          {events.map((e, i) => (
            <li key={`${e.id}-${i}`} className="run-event">
              <span
                className={`kind-badge kind-badge--${e.kind.toLowerCase()}`}
              >
                {KIND_LABEL[e.kind]}
              </span>
              <span className="run-event__actor">{e.actor}</span>
              {e.summary && (
                <span className="run-event__summary">{e.summary}</span>
              )}
              <time className="run-event__ts">{e.ts}</time>
            </li>
          ))}
        </ol>
      )}
    </section>
  );
}
