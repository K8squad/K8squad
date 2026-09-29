// SSE subscription over the ONE 8.2 EventSource / BFF proxy (AC6). The room
// does not open a bespoke socket: it rides the same `/api/projects/{id}/stream`
// channel every other live console surface uses (8.8f/8.10/8.11), filtered to
// this room's message events. If the feed slips, callers degrade to
// poll-on-focus (live is the target, not a hard gate).

import { encodeProjectId } from "@/lib/projectId";
import {
  parseRoomEvent,
  parseThinkingRow,
  type RoomStreamEvent,
} from "./liveFeed";

/** Minimal EventSource surface (so this is testable without a real browser). */
export interface EventSourceLike {
  addEventListener(type: string, listener: (e: { data: string }) => void): void;
  close(): void;
}

export type EventSourceFactory = (url: string) => EventSourceLike;

/** The shared 8.2 live stream URL for a Project (BFF proxied). */
export function streamUrl(projectId: string): string {
  return `/api/projects/${encodeProjectId(projectId)}/stream`;
}

/**
 * Subscribe to a thread's live events over the shared 8.2 project channel. The ONE
 * EventSource multiplexes two named events (§13 "one bus, no polling"):
 *   - `discussion` → a RoomEvent (message created/updated/deleted), filtered to
 *     this thread and delivered as `{kind:"room"}`;
 *   - `thinking`   → a live run thinking envelope (ISI-5208), delivered as
 *     `{kind:"thinking"}`. A thinking envelope carries no thread id, so it is NOT
 *     filtered here — the Room correlates it to a working watch by author (which is
 *     itself thread-scoped), dropping any that belong to no active dispatch.
 * Returns an unsubscribe function.
 */
export function subscribeRoom(
  projectId: string,
  threadId: string,
  onEvent: (evt: RoomStreamEvent) => void,
  makeSource: EventSourceFactory,
): () => void {
  const src = makeSource(streamUrl(projectId));
  const roomHandler = (e: { data: string }) => {
    const evt = parseRoomEvent(e.data);
    if (!evt) return;
    const tid =
      evt.type === "message.deleted" ? undefined : evt.message.threadId;
    // Deleted events lack a thread id in this minimal envelope; created/updated
    // carry threadId and are filtered to this thread. (A richer envelope can
    // carry threadId on delete too; then filter it the same way.)
    if (tid !== undefined && tid !== threadId) return;
    onEvent({ kind: "room", event: evt });
  };
  const thinkingHandler = (e: { data: string }) => {
    const row = parseThinkingRow(e.data);
    if (row) onEvent({ kind: "thinking", row });
  };
  src.addEventListener("discussion", roomHandler);
  src.addEventListener("thinking", thinkingHandler);
  return () => src.close();
}
