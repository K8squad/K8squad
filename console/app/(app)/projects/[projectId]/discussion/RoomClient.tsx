"use client";

// Client wrapper for the Discussion route: resolves the Project's room (R1 — the
// room IS the Project, keyed by projectId) as its thread list, picks the active
// thread, and mounts <DiscussionRoom> with a BFF-backed client and the shared
// 8.2 SSE subscription. Kept separate from page.tsx so the server component
// stays thin. There is no backend room object or `roomId`; the thread id is the
// sub-resource id (migration 0004 superseded the naive rooms shape).

import { useEffect, useMemo, useState } from "react";
import { createDiscussionClient } from "@/lib/discussion/api";
import { subscribeRoom, type EventSourceFactory } from "@/lib/discussion/sse";
import type { RoomEvent } from "@/lib/discussion/liveFeed";
import { DiscussionRoom } from "@/components/discussion/DiscussionRoom";

export function DiscussionRoomClient({ projectId }: { projectId: string }) {
  const client = useMemo(() => createDiscussionClient(), []);
  const [threadId, setThreadId] = useState<string | null>(null);

  useEffect(() => {
    let alive = true;
    void client.listThreads(projectId).then((threads) => {
      if (alive && threads.length > 0) setThreadId(threads[0].id);
    });
    return () => {
      alive = false;
    };
  }, [client, projectId]);

  const subscribe = useMemo(() => {
    if (typeof EventSource === "undefined" || threadId == null) return undefined;
    const factory: EventSourceFactory = (url) =>
      new EventSource(url) as unknown as ReturnType<EventSourceFactory>;
    return (onEvent: (evt: RoomEvent) => void) =>
      subscribeRoom(projectId, threadId, onEvent, factory);
  }, [projectId, threadId]);

  if (threadId == null) return <div data-testid="room-resolving">Loading…</div>;

  return (
    <DiscussionRoom
      projectId={projectId}
      threadId={threadId}
      client={client}
      subscribe={subscribe}
    />
  );
}
