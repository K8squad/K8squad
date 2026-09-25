"use client";

// DiscussionRoom — the Project-scoped Discussion view (AC1). Renders threaded
// history (agents + humans side by side), a top-level composer, and live-appends
// new messages arriving over the ONE 8.2 SSE channel (AC6). It is a pure
// consumer of the 10.1 API behind the BFF authz choke point: when the client
// reports `not-found` (the collapsed 401/403/404 deny, AC4) it renders "missing"
// and zero threads — never another Team's content.
//
// Discussion Room v2 (ISI-4929, plan §4.2/§4.5 story 5): the room gains the
// agent roster with presence (direct-target selector + roster rail) and the
// composer's audience selector + `@`-mention popover. Audience scoping stays a
// SERVER decision (Store reads); the room only renders what it is given.

import { useCallback, useEffect, useMemo, useState } from "react";
import type { MentionSuggestion, Message } from "@/lib/discussion/types";
import { nestMessages } from "@/lib/discussion/thread";
import { applyRoomEvent, type RoomEvent } from "@/lib/discussion/liveFeed";
import type { DiscussionClient } from "@/lib/discussion/api";
import { DiscussionApiError } from "@/lib/discussion/api";
import { MessageItem } from "./MessageItem";
import { Composer } from "./Composer";
import { Roster, type RosterAgent } from "./Roster";
import "./discussion.css";

export interface DiscussionRoomProps {
  projectId: string;
  threadId: string;
  client: DiscussionClient;
  /**
   * Subscribe to the thread's live event stream (the 8.2 EventSource/BFF proxy).
   * Returns an unsubscribe fn. Optional — absent means poll-on-focus degrade.
   */
  subscribe?: (onEvent: (evt: RoomEvent) => void) => () => void;
  /**
   * Roster loader (ISI-4929, plan §4.5): given the thread's Team id, resolves
   * the agent roster the sidebar + composer direct-target selector render.
   * Optional — absent degrades to a roster-less room (party-only composer).
   */
  loadRoster?: (teamId: string) => Promise<RosterAgent[]>;
  /** Mention search backing the composer's `@` popover (ISI-4926 endpoint). */
  searchMentions?: (q: string) => Promise<MentionSuggestion[]>;
}

type LoadState = "loading" | "ready" | "not-found" | "error";

export function DiscussionRoom({
  projectId,
  threadId,
  client,
  subscribe,
  loadRoster,
  searchMentions,
}: DiscussionRoomProps) {
  const [state, setState] = useState<LoadState>("loading");
  const [messages, setMessages] = useState<Message[]>([]);
  const [rosterAgents, setRosterAgents] = useState<RosterAgent[]>([]);

  const load = useCallback(async () => {
    try {
      const flat = await client.getThread(projectId, threadId);
      setMessages(flat);
      setState("ready");
      // The thread's Team scopes the roster (§4.5). A roster failure degrades
      // silently to a roster-less room — the thread itself already rendered.
      try {
        const thread = await client.getThreadInfo(projectId, threadId);
        if (loadRoster && thread.teamId) {
          const agents = await loadRoster(thread.teamId);
          setRosterAgents(agents);
        }
      } catch {
        setRosterAgents([]);
      }
    } catch (err) {
      if (err instanceof DiscussionApiError && err.outcome === "not-found") {
        setState("not-found");
        setMessages([]); // never render foreign threads
      } else {
        setState("error");
      }
    }
  }, [client, projectId, threadId, loadRoster]);

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
    async (body: { body: string; parentId?: string; audience?: string }) => {
      const created = await client.postMessage(projectId, threadId, body);
      // Optimistic upsert; the SSE echo is deduped by id.
      setMessages((cur) =>
        applyRoomEvent(cur, { type: "message.created", message: created }),
      );
    },
    [client, projectId, threadId],
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
    return (
      <div data-testid="room-error">Something went wrong loading the room.</div>
    );
  }

  return (
    <section className="ksq-room" data-testid="discussion-room">
      <div className="ksq-room__main">
        <ul className="ksq-thread ksq-thread--roots" data-testid="threads">
          {threads.map((t) => (
            <MessageItem key={t.id} message={t} />
          ))}
        </ul>
        <Composer
          onPost={post}
          directTargets={rosterAgents.map((a) => ({ id: a.id, name: a.name }))}
          searchMentions={searchMentions}
        />
      </div>
      <Roster agents={rosterAgents} />
    </section>
  );
}
