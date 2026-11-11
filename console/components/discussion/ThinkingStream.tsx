"use client";

// ThinkingStream — the discussion Room's live run-activity surface (ISI-5208,
// demoted in ISI-5696, TWO-TIER in ISI-7101/WS-4).
//
// Rendered beneath a message that @-mentioned an agent: while the dispatched run
// is active, its `thinking` envelopes stream in over the ONE project SSE channel
// (bridged from the run progress mirror, ISI-5193) and collect here in arrival
// order. It is a pure projection of the correlated rows
// (lib/discussion/liveThinking.ts); the durable reply lands separately as its own
// message once the run posts back — that message IS the readable transcript.
//
// ISI-7101 (WS-4 — "render two tiers: live italic thinking, one clean comment"):
// the stream now renders the Paperclip two-tier model on the discussion surface.
//   - WHILE THE RUN IS WORKING (active): the expander is OPEN by default and
//     labelled "working…" — the live reasoning streams visibly in italic (the
//     in-progress tier), so a watcher sees the agent think. This lifts the
//     ISI-5696 total demotion ONLY for the live window it was over-broad about:
//     a reader who closed it stays closed, and once the run completes the block
//     falls back to the collapsed "Run activity" affordance.
//   - ON COMPLETION (active false): collapsed by default (the ISI-5696
//     behavior — the posted reply is the transcript; the raw activity stays one
//     click away), rows still italic (ephemeral tier, clearly not prose).
//   - DELIVERABLE TIER (ISI-7098 wire kind "deliverable"): the run ONE final
//     answer, arriving live before the durable reply message lands — rendered
//     UPRIGHT above the italic stream (it is the clean comment, never thinking).
//     Absent kind = legacy envelope = thinking, per the wire rule.
//
// Reasoning-TEXT depth stays gated on ISI-4812: this renders the
// envelope/liveness body the mirror already surfaces — never raw model reasoning.

import type { ThinkingRow } from "@/lib/discussion/liveFeed";
import { authorAgentName } from "@/lib/discussion/liveThinking";
import { cleanActivityBody } from "@/lib/discussion/activityView";

export function ThinkingStream({
  rows,
  active = false,
}: {
  rows?: readonly ThinkingRow[];
  /**
   * True while the run this stream belongs to is still working (the Room derives
   * it from the message's dispatch watches). Drives the two-tier live behavior:
   * open + "working…" while active, collapsed "Run activity" once settled.
   */
  active?: boolean;
}) {
  if (!rows || rows.length === 0) return null;
  // ISI-7098 tier split: deliverable rows (the run ONE final answer) render
  // upright above the italic stream; thinking rows (and legacy un-stamped ones)
  // stay in the italic in-progress list.
  const deliverables = rows.filter((r) => r.kind === "deliverable");
  const thinking = rows.filter((r) => r.kind !== "deliverable");
  return (
    <details
      className="ksq-activity"
      data-testid="activity-details"
      open={active || undefined}
    >
      <summary className="ksq-activity__summary" data-testid="activity-toggle">
        {active ? "working…" : "Run activity"} · {rows.length}
      </summary>
      {deliverables.length > 0 ? (
        <ul
          className="ksq-thinking-list ksq-thinking-list--final"
          data-testid="thinking-final"
        >
          {deliverables.map((r, i) => (
            <li
              key={`final:${r.runId}:${r.seq}:${i}`}
              className="ksq-thinking ksq-thinking--final"
              data-testid="thinking-final-row"
              data-run-id={r.runId}
            >
              <span className="ksq-thinking__author">
                {authorAgentName(r.author)}
              </span>
              <span className="ksq-thinking__body ksq-thinking__body--final">
                {cleanActivityBody(r.body)}
              </span>
            </li>
          ))}
        </ul>
      ) : null}
      <ul
        className="ksq-thinking-list"
        data-testid="thinking-list"
        aria-live={active ? "polite" : undefined}
        role="log"
      >
        {thinking.map((r, i) => (
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
