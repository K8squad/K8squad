"use client";

// ThinkingStream — the discussion Room's live "agent thinking" surface (ISI-5208).
//
// Rendered beneath a message that @-mentioned an agent, alongside the working
// chip: while the dispatched run is active, its `thinking` envelopes stream in over
// the ONE project SSE channel (bridged from the run progress mirror, ISI-5193) and
// append here in arrival order — so the room streams the agent's progress live
// instead of spinning until a reload. It is a pure projection of the correlated
// rows (lib/discussion/liveThinking.ts); the durable reply lands separately as its
// own message once the run posts back.
//
// Reasoning-TEXT depth stays gated on ISI-4812: this renders the envelope/liveness
// body the mirror already surfaces (e.g. "[run …] tool: …"), the same policy the
// ticket feed applies — never raw model reasoning.

import type { ThinkingRow } from "@/lib/discussion/liveFeed";
import { authorAgentName } from "@/lib/discussion/liveThinking";

export function ThinkingStream({ rows }: { rows?: readonly ThinkingRow[] }) {
  if (!rows || rows.length === 0) return null;
  return (
    <ul
      className="ksq-thinking-list"
      data-testid="thinking-list"
      aria-live="polite"
      role="log"
    >
      {rows.map((r, i) => (
        <li
          key={`${r.runId}:${r.seq}:${i}`}
          className="ksq-thinking"
          data-testid="thinking-row"
          data-run-id={r.runId}
        >
          <span className="ksq-thinking__author">{authorAgentName(r.author)}</span>
          <span className="ksq-thinking__body">{r.body}</span>
        </li>
      ))}
    </ul>
  );
}
