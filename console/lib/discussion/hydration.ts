// Durable REST hydration for the discussion-room feed (ISI-7869, WS-C of
// parent ISI-7864) — the discussion analog of the ISI-7484 run Activity
// fallback. The live feed's run-scoped rows (thinking envelopes,
// ticket_comment echoes, discussion_message echoes) ride SSE only and carry
// no thread/message id, so the Room correlates them to its LIVE working
// watches — a fresh tab has none and renders an empty step feed. This module
// fetches the thread's run-event tail (the same coord.outbox journal the
// projector bridges, keyed by the anchor message each run answers) and shapes
// it with the SAME parsers/reducers the live bus uses, so hydration and live
// frames fold identically and dedupe by the keys the reducers already keep
// (message id; (runId, seq) per thinking row). Transport-free pure shaping
// plus one best-effort fetch; any failure degrades to the SSE-only behavior
// of before this seam existed.

import { encodeProjectId } from "@/lib/projectId";
import {
  parseRoomEvent,
  parseThinkingRow,
  parseTicketCommentRow,
  ticketCommentToDeliverable,
  type RoomEvent,
  type ThinkingRow,
} from "./liveFeed";

/** One row of the hydration tail — the ISI-7484 quartet (id/type/data/at). */
export interface HydratedRunEvent {
  id: string;
  type: string;
  data: unknown;
  at: string;
}

/** One dispatched run's hydration slice (anchor = the dispatch message). */
export interface HydratedThreadRun {
  runId: string;
  anchorMessageId: string;
  events: HydratedRunEvent[];
}

function isHydratedRunEvent(v: unknown): v is HydratedRunEvent {
  if (!v || typeof v !== "object") return false;
  const e = v as Record<string, unknown>;
  return typeof e.id === "string" && typeof e.type === "string";
}

/**
 * Best-effort fetch of the thread's durable run-event tail. null means
 * degrade: the room keeps its live SSE behavior and never blocks rendering
 * on hydration.
 */
export async function fetchThreadRunEvents(
  projectId: string,
  threadId: string,
): Promise<HydratedThreadRun[] | null> {
  try {
    const res = await fetch(
      `/api/projects/${encodeProjectId(
        projectId,
      )}/discussion/threads/${encodeURIComponent(threadId)}/run-events`,
      { headers: { accept: "application/json" }, cache: "no-store" },
    );
    if (!res.ok) return null;
    const body = (await res.json()) as { runs?: unknown };
    if (!body || typeof body !== "object" || !Array.isArray(body.runs)) {
      return null;
    }
    const runs: HydratedThreadRun[] = [];
    for (const r of body.runs) {
      if (!r || typeof r !== "object") continue;
      const e = r as Record<string, unknown>;
      if (
        typeof e.runId !== "string" ||
        typeof e.anchorMessageId !== "string" ||
        !Array.isArray(e.events)
      ) {
        continue;
      }
      runs.push({
        runId: e.runId,
        anchorMessageId: e.anchorMessageId,
        events: e.events.filter(isHydratedRunEvent),
      });
    }
    return runs;
  } catch {
    return null;
  }
}

/** Shape one event's payload for the live parsers (they take a raw string). */
function rowPayload(row: HydratedRunEvent): string {
  return typeof row.data === "string"
    ? row.data
    : JSON.stringify(row.data ?? {});
}

/**
 * Seed the per-message live-thinking map from the hydration tail. Each run's
 * rows fold under its ANCHOR message — the durable correlation the live wire
 * lacks (an SSE envelope carries no thread/message id and is matched to a
 * working watch by author, which a fresh tab does not have) — through the
 * same mappers the live handlers use, so a hydrated row renders identically
 * to its live twin. De-duped by (runId, seq) against `current` in both
 * directions: a live frame that already landed never double-renders, and a
 * hydrated row never double-renders against the live map. Input map is not
 * mutated; event order (the outbox commit order the tail serves ascending)
 * is preserved, so the hydrated feed reads in the order it happened.
 */
export function seedLiveThinking(
  runs: readonly HydratedThreadRun[],
  current: Readonly<Record<string, ThinkingRow[]>>,
): Record<string, ThinkingRow[]> {
  const next: Record<string, ThinkingRow[]> = { ...current };
  for (const run of runs) {
    const merged = [...(next[run.anchorMessageId] ?? [])];
    for (const ev of run.events) {
      let row: ThinkingRow | null = null;
      if (ev.type === "thinking") {
        row = parseThinkingRow(rowPayload(ev));
      } else if (ev.type === "ticket_comment") {
        const comment = parseTicketCommentRow(rowPayload(ev));
        if (comment) row = ticketCommentToDeliverable(comment);
      }
      if (!row) continue;
      if (merged.some((r) => r.runId === row!.runId && r.seq === row!.seq)) {
        continue;
      }
      merged.push(row);
    }
    // Only materialize a bucket when it carries rows: an anchor whose tail
    // held nothing renderable leaves no empty entry in the map.
    if (merged.length > 0) {
      next[run.anchorMessageId] = merged;
    }
  }
  return next;
}

/**
 * Extract the room message events from the hydration tail (the
 * `discussion_message` echoes) as RoomEvents, to fold through
 * applyRoomEvent — idempotent by message id, so a row the thread read already
 * returned (or a live frame already applied) is a no-op. This closes the
 * load/live seam race: a message committed between the thread GET and the
 * SSE subscribe still hydrates.
 */
export function hydratedRoomEvents(
  runs: readonly HydratedThreadRun[],
): RoomEvent[] {
  const out: RoomEvent[] = [];
  for (const run of runs) {
    for (const ev of run.events) {
      if (ev.type !== "discussion_message") continue;
      const evt = parseRoomEvent(rowPayload(ev));
      if (evt) out.push(evt);
    }
  }
  return out;
}
