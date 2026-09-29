// Live thinking correlation (ISI-5208, plan ISI-5207) — the pure state behind the
// discussion Room's "stream the agent's thinking inline while the run is active"
// ask. A `thinking` envelope (lib/discussion/liveFeed.ts#ThinkingRow) arrives on
// the per-project SSE bus with a run-stamped author but NO thread/message id, so
// the Room cannot key it directly. This module correlates each envelope to the
// dispatch working watch it belongs under — the SAME author-identity correlation
// working.ts#applyReply uses to resolve a reply — and folds it into a per-message
// map the thread renders beneath the triggering message.
//
// It is transport-free and opens no stream (§13 "one bus, no polling"): the Room
// feeds it the rows arriving over the ONE project EventSource. Reasoning-TEXT depth
// stays gated on ISI-4812; this is the envelope/liveness signal only.

import type { ThinkingRow } from "./liveFeed";
import type { DispatchWatch } from "./working";

/** Strip the run-stamped author prefix (`agent:{name}` → `{name}`; `run/{id}`
 * stays as-is and matches no roster agent) so the identity compares against a
 * working watch's roster `agentName`. */
export function authorAgentName(author: string): string {
  const a = author ?? "";
  return a.startsWith("agent:") ? a.slice("agent:".length) : a;
}

/**
 * Resolve which triggering message a thinking envelope renders under, by matching
 * its author to a still-`working` dispatch watch — mirroring working.ts#applyReply:
 *   - author matches a working watch's agent NAME (case-insensitive) ⇒ that watch's
 *     message; if several match, the most recently seeded one (newest `since`);
 *   - no name match but EXACTLY ONE watch is still working ⇒ that lone watch (a run
 *     is live and there is only one thing this can be);
 *   - otherwise ⇒ null (ambiguous, or the envelope is for a run this thread did not
 *     dispatch — e.g. a ticket run in the same project — so it is dropped).
 * This author-correlation IS the thread filter: a `thinking` envelope carries no
 * thread id, but a working watch is thread-scoped, so only this thread's active
 * dispatches ever match.
 */
export function correlateThinking(
  watches: readonly DispatchWatch[],
  author: string,
): string | null {
  const name = authorAgentName(author).toLowerCase();
  const active = watches.filter((w) => w.phase === "working");
  if (active.length === 0) return null;

  const byName = active.filter((w) => w.agentName.toLowerCase() === name);
  if (byName.length > 0) {
    return byName.reduce((a, b) => (b.since >= a.since ? b : a)).messageId;
  }
  if (active.length === 1) return active[0].messageId;
  return null;
}

/**
 * Fold one thinking envelope into the per-message live-thinking map. The row is
 * appended under the message its author correlates to (correlateThinking); an
 * uncorrelated row is dropped (map returned unchanged). De-duped within a message
 * by (runId, seq) so an SSE replay/redelivery never double-renders a row. Input
 * map is not mutated.
 */
export function appendThinking(
  current: Readonly<Record<string, ThinkingRow[]>>,
  row: ThinkingRow,
  watches: readonly DispatchWatch[],
): Record<string, ThinkingRow[]> {
  const messageId = correlateThinking(watches, row.author);
  if (!messageId) return { ...current };

  const existing = current[messageId] ?? [];
  if (existing.some((r) => r.runId === row.runId && r.seq === row.seq)) {
    return { ...current };
  }
  return { ...current, [messageId]: [...existing, row] };
}
