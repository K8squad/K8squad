// Live append over the ONE 8.2 SSE / BFF proxy (Story 10.3 AC6). There is no
// bespoke second live channel: the room rides the same EventSource every other
// live surface uses (8.8f/8.10/8.11). This module is the pure reducer that
// folds a room event into the flat message list — idempotent by message id so
// that the server echo of a just-posted message (or an SSE replay) never
// duplicates a row.

import type { Message } from "./types";

export type RoomEvent =
  | { type: "message.created"; message: Message }
  | { type: "message.updated"; message: Message }
  | { type: "message.deleted"; id: string }
  // ISI-5272: a discussion-dispatched run reached a NO-REPLY terminal state
  // (failed/cancelled), emitted by the apiserver run-event projector onto the
  // per-project bus. It commits no reply message, so the `message.created` echo
  // that normally resolves the working watch never arrives; this event flips the
  // matching watch → "failed" immediately instead of waiting for the client
  // timeout. `agentName` is the FE-side correlation key (the minted run id never
  // reaches the console); `messageId` is the triggering post when known.
  | { type: "dispatch.failed"; agentName: string; messageId?: string };

/**
 * One live `thinking` envelope streamed off a run's progress mirror (ISI-5193),
 * bridged onto the per-project SSE bus so the discussion Room can render it inline
 * while a dispatched run is active (ISI-5208). It is the SAME triple the durable
 * coord.comment row holds plus the run id/seq: {runId, author, body, seq, at}.
 * `author` is the run-stamped identity (`agent:{name}` or `run/{shortId}`) — the
 * Room correlates it to a working watch by the agent NAME to know which message it
 * belongs under (a thinking envelope carries no thread/message id). Reasoning-TEXT
 * depth stays gated on ISI-4812 — this is the envelope/liveness signal only, the
 * same policy as the ticket feed.
 */
export interface ThinkingRow {
  runId: string;
  author: string;
  body: string;
  seq: number;
  at: string;
}

/**
 * The discriminated event the room's ONE project EventSource delivers: a room
 * message change (the `discussion` named event) or a live thinking envelope (the
 * `thinking` named event). Both ride the single 8.2 stream — no second transport.
 */
export type RoomStreamEvent =
  | { kind: "room"; event: RoomEvent }
  | { kind: "thinking"; row: ThinkingRow };

/** Parse a raw `thinking` SSE `data:` payload into a ThinkingRow, or null when it
 * carries no renderable body (author or body missing ⇒ nothing to show). */
export function parseThinkingRow(raw: string): ThinkingRow | null {
  let obj: unknown;
  try {
    obj = JSON.parse(raw);
  } catch {
    return null;
  }
  if (!obj || typeof obj !== "object") return null;
  const e = obj as Record<string, unknown>;
  const body = typeof e.body === "string" ? e.body : "";
  if (!body) return null;
  return {
    runId: typeof e.runId === "string" ? e.runId : "",
    author: typeof e.author === "string" ? e.author : "",
    body,
    seq: typeof e.seq === "number" ? e.seq : 0,
    at: typeof e.at === "string" ? e.at : "",
  };
}

/**
 * Insert or replace a message by id, keeping the list ordered by `createdAt`
 * then `id`. Idempotent: applying the same message twice is a no-op beyond the
 * single upsert. Input list is not mutated.
 */
export function upsertMessage(list: readonly Message[], m: Message): Message[] {
  const next = list.filter((x) => x.id !== m.id);
  next.push(m);
  next.sort((a, b) =>
    a.createdAt !== b.createdAt
      ? a.createdAt < b.createdAt
        ? -1
        : 1
      : a.id < b.id
        ? -1
        : a.id > b.id
          ? 1
          : 0,
  );
  return next;
}

/** Fold a single room event into the flat message list. */
export function applyRoomEvent(
  list: readonly Message[],
  evt: RoomEvent,
): Message[] {
  switch (evt.type) {
    case "message.created":
    case "message.updated":
      return upsertMessage(list, evt.message);
    case "message.deleted":
      return list.filter((x) => x.id !== evt.id);
    case "dispatch.failed":
      // A run-terminal signal carries no message — it only affects the working
      // watch set (handled by the room component via applyDispatchFailed), so the
      // flat message list is unchanged.
      return list as Message[];
    default: {
      // Exhaustiveness guard: unknown events are ignored, never throw.
      return list as Message[];
    }
  }
}

/** Parse a raw SSE `data:` payload into a RoomEvent, or null if unrecognized. */
export function parseRoomEvent(raw: string): RoomEvent | null {
  let obj: unknown;
  try {
    obj = JSON.parse(raw);
  } catch {
    return null;
  }
  if (!obj || typeof obj !== "object") return null;
  const e = obj as Record<string, unknown>;
  if (e.type === "message.deleted" && typeof e.id === "string") {
    return { type: "message.deleted", id: e.id };
  }
  if (e.type === "dispatch.failed" && typeof e.agentName === "string" && e.agentName) {
    return {
      type: "dispatch.failed",
      agentName: e.agentName,
      ...(typeof e.messageId === "string" && e.messageId
        ? { messageId: e.messageId }
        : {}),
    };
  }
  if (
    (e.type === "message.created" || e.type === "message.updated") &&
    e.message &&
    typeof e.message === "object"
  ) {
    return {
      type: e.type,
      message: e.message as Message,
    };
  }
  return null;
}
