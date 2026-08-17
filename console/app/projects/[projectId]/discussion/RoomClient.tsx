'use client';

// Client wrapper for the Discussion route: resolves the Project's default room
// and mounts <DiscussionRoom> with a BFF-backed client and the shared 8.2 SSE
// subscription. Kept separate from page.tsx so the server component stays thin.

import { useEffect, useMemo, useState } from 'react';
import { createDiscussionClient } from '@/lib/discussion/api';
import { subscribeRoom, type EventSourceFactory } from '@/lib/discussion/sse';
import type { RoomEvent } from '@/lib/discussion/liveFeed';
import { DiscussionRoom } from '@/components/discussion/DiscussionRoom';

export function DiscussionRoomClient({ projectId }: { projectId: string }) {
  const client = useMemo(() => createDiscussionClient(), []);
  const [roomId, setRoomId] = useState<string | null>(null);

  useEffect(() => {
    let alive = true;
    void client.listRooms(projectId).then((rooms) => {
      if (alive && rooms.length > 0) setRoomId(rooms[0].id);
    });
    return () => {
      alive = false;
    };
  }, [client, projectId]);

  const subscribe = useMemo(() => {
    if (typeof EventSource === 'undefined' || roomId == null) return undefined;
    const factory: EventSourceFactory = (url) =>
      new EventSource(url) as unknown as ReturnType<EventSourceFactory>;
    return (onEvent: (evt: RoomEvent) => void) =>
      subscribeRoom(projectId, roomId, onEvent, factory);
  }, [projectId, roomId]);

  if (roomId == null) return <div data-testid="room-resolving">Loading…</div>;

  return (
    <DiscussionRoom
      projectId={projectId}
      roomId={roomId}
      client={client}
      subscribe={subscribe}
    />
  );
}
