"use client";

// Client wrapper for the Discussion route: resolves the Project's room (R1 — the
// room IS the Project, keyed by projectId) as its thread list, picks the active
// thread, and mounts <DiscussionRoom> with a BFF-backed client and the shared
// 8.2 SSE subscription. Kept separate from page.tsx so the server component
// stays thin. There is no backend room object or `roomId`; the thread id is the
// sub-resource id (migration 0004 superseded the naive rooms shape).
//
// ISI-4929: also wires the roster loader (Team org read model → the room's
// roster/presence sidebar + the composer's direct-target selector) and the
// mention search (ISI-4926 endpoint) into the room. Both are optional props on
// <DiscussionRoom>; failures degrade to a roster-less, popover-less composer.

import { useCallback, useEffect, useMemo, useState } from "react";
import { createDiscussionClient } from "@/lib/discussion/api";
import { subscribeRoom, type EventSourceFactory } from "@/lib/discussion/sse";
import type { RoomEvent } from "@/lib/discussion/liveFeed";
import type { RosterAgent } from "@/components/discussion/Roster";
import { createAgentsClient } from "@/lib/agents/api";
import { DiscussionRoom } from "@/components/discussion/DiscussionRoom";

export function DiscussionRoomClient({ projectId }: { projectId: string }) {
  const client = useMemo(() => createDiscussionClient(), []);
  const agentsClient = useMemo(() => createAgentsClient(), []);
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

  // Roster: the Team org read model (8.10) projected onto the room's roster
  // rows. A Team read failure rejects and the room degrades to roster-less.
  const loadRoster = useCallback(
    (teamId: string): Promise<RosterAgent[]> =>
      agentsClient
        .getTeamOrg(teamId)
        .then((org) =>
          org.agents.map((a) => ({
            id: a.id,
            name: a.name,
            status: a.status,
          })),
        ),
    [agentsClient],
  );

  const searchMentions = useCallback(
    (q: string) => client.searchMentions(projectId, q),
    [client, projectId],
  );

  if (threadId == null) return <div data-testid="room-resolving">Loading…</div>;

  return (
    <DiscussionRoom
      projectId={projectId}
      threadId={threadId}
      client={client}
      subscribe={subscribe}
      loadRoster={loadRoster}
      searchMentions={searchMentions}
    />
  );
}
