"use client";

// ThinkingStream — the discussion Room's live run-activity surface (ISI-5208,
// demoted in ISI-5696).
//
// Rendered beneath a message that @-mentioned an agent: while the dispatched run
// is active, its `thinking` envelopes stream in over the ONE project SSE channel
// (bridged from the run progress mirror, ISI-5193) and collect here in arrival
// order. It is a pure projection of the correlated rows
// (lib/discussion/liveThinking.ts); the durable reply lands separately as its own
// message once the run posts back — that message IS the readable transcript.
//
// ISI-5696 — surface replies, DEMOTE this stream. Henrik's party-mode feedback:
// the room was flooding the transcript with the raw per-run EXECUTION stream
// (`[run …][tool:…/result][server:…]` markers, `[untrusted]` reasoning,
// `[status]` lines) instead of the agents' actual posted replies. So this is no
// longer a top-level live log. The compact in-flight signal is the WorkingIndicator
// ("{agent} is working…" → "replied") rendered alongside; the raw activity is
// folded here behind a per-message collapsible "Run activity" affordance, CLOSED
// by default, with the `[run]/[tool]/[server]/[untrusted]/[status]` prefixes
// stripped (lib/discussion/activityView.ts) so the deep-dive reads as plain
// progress text rather than wire markers. Nothing from this stream is visible in
// the default transcript until a reader opens the toggle.
//
// Reasoning-TEXT depth stays gated on ISI-4812: this renders the envelope/liveness
// body the mirror already surfaces — never raw model reasoning.

import type { ThinkingRow } from "@/lib/discussion/liveFeed";
import { authorAgentName } from "@/lib/discussion/liveThinking";
import { cleanActivityBody } from "@/lib/discussion/activityView";

export function ThinkingStream({ rows }: { rows?: readonly ThinkingRow[] }) {
  if (!rows || rows.length === 0) return null;
  return (
    <details className="ksq-activity" data-testid="activity-details">
      <summary className="ksq-activity__summary" data-testid="activity-toggle">
        Run activity · {rows.length}
      </summary>
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
            <span className="ksq-thinking__author">
              {authorAgentName(r.author)}
            </span>
            <span className="ksq-thinking__body">
              {cleanActivityBody(r.body)}
            </span>
          </li>
        ))}
      </ul>
    </details>
  );
}
