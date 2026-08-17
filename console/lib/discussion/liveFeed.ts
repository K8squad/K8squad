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
  | { type: "message.deleted"; id: string };

/**
 * Insert or replace a message by id, keeping the list ordered by `createdAt`
 * then `id`. Idempotent: applying the same message twice is a no-op beyond the
 * single upsert. Input list is not mutated.
 */
export function upsertMessage(
  list: readonly Message[],
  m: Message,
): Message[] {
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
