"use client";

// components/RunStream.tsx — the live Run-progress timeline (story 8.2).
//
// Consumes the ONE shared EventSource (lib/useRunStream) and renders the coordination-event
// timeline: kind badge + actor·role + mono timestamp. READ-ONLY legibility — no claim/mutate/
// transition control rides the feed (AC6). Kill Run (FR-F4) is a separate control-plane action
// owned by story 3.3/8.4, deliberately NOT a stream verb, so it is absent here.

import { useRunStream, type RunEventKind } from "@/lib/useRunStream";

const KIND_LABEL: Record<RunEventKind, string> = {
  CHECKOUT: "checkout",
  COMMENT: "comment",
  HANDOFF: "handoff",
  MEMORY: "memory",
  ARTIFACT: "artifact",
};

export function RunStream({ runId }: { runId: string }) {
  const { events, status } = useRunStream(runId);

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
