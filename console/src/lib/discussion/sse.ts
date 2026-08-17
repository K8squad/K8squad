// SSE subscription over the ONE 8.2 EventSource / BFF proxy (AC6). The room
// does not open a bespoke socket: it rides the same `/api/projects/{id}/stream`
// channel every other live console surface uses (8.8f/8.10/8.11), filtered to
// this room's message events. If the feed slips, callers degrade to
// poll-on-focus (live is the target, not a hard gate).

import { parseRoomEvent, type RoomEvent } from "./liveFeed";

/** Minimal EventSource surface (so this is testable without a real browser). */
export interface EventSourceLike {
  addEventListener(type: string, listener: (e: { data: string }) => void): void;
  close(): void;
}

export type EventSourceFactory = (url: string) => EventSourceLike;

/** The shared 8.2 live stream URL for a Project (BFF proxied). */
export function streamUrl(projectId: string): string {
  return `/api/projects/${encodeURIComponent(projectId)}/stream`;
}

/**
 * Subscribe to a room's live message events over the shared 8.2 channel.
 * Returns an unsubscribe function. Events for other rooms are ignored.
 */
export function subscribeRoom(
  projectId: string,
  roomId: string,
  onEvent: (evt: RoomEvent) => void,
  makeSource: EventSourceFactory,
): () => void {
  const src = makeSource(streamUrl(projectId));
  const handler = (e: { data: string }) => {
    const evt = parseRoomEvent(e.data);
    if (!evt) return;
    const mid =
      evt.type === "message.deleted" ? undefined : evt.message.roomId;
    // Deleted events lack a room id in this minimal envelope; created/updated
    // carry roomId and are filtered to this room. (A richer envelope can carry
    // roomId on delete too; then filter it the same way.)
    if (mid !== undefined && mid !== roomId) return;
    onEvent(evt);
  };
  src.addEventListener("discussion", handler);
  return () => src.close();
}
