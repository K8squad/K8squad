// DiscussionRoom — the Project-scoped Discussion view (AC1). Renders threaded
// history (agents + humans side by side), a top-level composer, and live-appends
// new messages arriving over the ONE 8.2 SSE channel (AC6). It is a pure
// consumer of the 10.1 API behind the BFF authz choke point: when the client
// reports `not-found` (the collapsed 401/403/404 deny, AC4) it renders "missing"
// and zero threads — never another Team's content.
//
// NO coordination affordance is rendered anywhere in this surface (AC5).

import { useCallback, useEffect, useMemo, useState } from "react";
import type { Message } from "../../lib/discussion/types";
import { nestMessages } from "../../lib/discussion/thread";
import { applyRoomEvent, type RoomEvent } from "../../lib/discussion/liveFeed";
import type { DiscussionClient } from "../../lib/discussion/api";
import { DiscussionApiError } from "../../lib/discussion/api";
import { MessageItem } from "./MessageItem";
import { Composer } from "./Composer";
import "./discussion.css";

export interface DiscussionRoomProps {
  projectId: string;
  roomId: string;
  client: DiscussionClient;
  /**
   * Subscribe to the room's live event stream (the 8.2 EventSource/BFF proxy).
   * Returns an unsubscribe fn. Optional — absent means poll-on-focus degrade.
   */
  subscribe?: (onEvent: (evt: RoomEvent) => void) => () => void;
}

type LoadState = "loading" | "ready" | "not-found" | "error";

export function DiscussionRoom({
  projectId,
  roomId,
  client,
  subscribe,
}: DiscussionRoomProps) {
  const [state, setState] = useState<LoadState>("loading");
  const [messages, setMessages] = useState<Message[]>([]);

  const load = useCallback(async () => {
    try {
      const flat = await client.getMessages(projectId, roomId, {
        threadDepth: 100,
      });
      setMessages(flat);
      setState("ready");
    } catch (err) {
      if (err instanceof DiscussionApiError && err.outcome === "not-found") {
        setState("not-found");
        setMessages([]); // never render foreign threads
      } else {
        setState("error");
      }
    }
  }, [client, projectId, roomId]);

  useEffect(() => {
    void load();
  }, [load]);

  // Live append over the single 8.2 SSE channel (idempotent by id).
  useEffect(() => {
    if (!subscribe) return;
    const unsub = subscribe((evt) => {
      setMessages((cur) => applyRoomEvent(cur, evt));
    });
    return unsub;
  }, [subscribe]);

  const threads = useMemo(() => nestMessages(messages), [messages]);

  const post = useCallback(
    async (body: { body: string; parentId?: string }) => {
      const created = await client.postMessage(projectId, roomId, body);
      // Optimistic upsert; the SSE echo is deduped by id.
      setMessages((cur) => applyRoomEvent(cur, { type: "message.created", message: created }));
    },
    [client, projectId, roomId],
  );

  if (state === "loading") {
    return <div data-testid="room-loading">Loading discussion…</div>;
  }
  if (state === "not-found") {
    return (
      <div data-testid="room-not-found">
        <h1>Not found</h1>
        <p>This discussion does not exist or you don’t have access.</p>
      </div>
    );
  }
  if (state === "error") {
    return <div data-testid="room-error">Something went wrong loading the room.</div>;
  }

  return (
    <section className="ksq-room" data-testid="discussion-room">
      <ul className="ksq-thread ksq-thread--roots" data-testid="threads">
        {threads.map((t) => (
          <MessageItem key={t.id} message={t} />
        ))}
      </ul>
      <Composer onPost={post} />
    </section>
  );
}
