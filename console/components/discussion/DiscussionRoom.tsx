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

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { MentionSuggestion, Message, Proposal } from "@/lib/discussion/types";
import { nestMessages } from "@/lib/discussion/thread";
import {
  applyRoomEvent,
  type RoomStreamEvent,
  type ThinkingRow,
} from "@/lib/discussion/liveFeed";
import { appendThinking } from "@/lib/discussion/liveThinking";
import { audienceWire } from "@/lib/discussion/audience";
import {
  applyDispatchFailed,
  applyReply,
  dispatchTargets,
  expireStale,
  groupByMessage,
  hasActiveWork,
  seedWatches,
  type DispatchWatch,
} from "@/lib/discussion/working";
import type { DiscussionClient } from "@/lib/discussion/api";
import { DiscussionApiError } from "@/lib/discussion/api";
import { MessageItem } from "./MessageItem";
import { Composer } from "./Composer";
import { Roster, type RosterAgent } from "./Roster";
import "./discussion.css";

// How long a dispatched agent may stay silent before the room shows "could not
// respond" (ISI-5174 AC3). A discussion thread-run reads the thread, runs a
// model turn, and posts back — minutes, not seconds — so this is generous enough
// not to false-fail a live run yet short enough that a dead run stops spinning.
const WORKING_TIMEOUT_MS = 5 * 60_000;
// Cadence at which the room re-checks working watches against the clock. The
// reducer (expireStale) is pure in `now`; this timer just supplies the tick.
const WORKING_SWEEP_MS = 15_000;

export interface DiscussionRoomProps {
  projectId: string;
  threadId: string;
  client: DiscussionClient;
  /**
   * Subscribe to the thread's live event stream (the 8.2 EventSource/BFF proxy).
   * Delivers the multiplexed union — room message changes AND live run `thinking`
   * envelopes (ISI-5208) — off the ONE project channel. Returns an unsubscribe fn.
   * Optional — absent means poll-on-focus degrade.
   */
  subscribe?: (onEvent: (evt: RoomStreamEvent) => void) => () => void;
  /**
   * Roster loader (ISI-4929, plan §4.5; project-scoped in ISI-5107): resolves
   * the agents dispatchable into THIS project for the sidebar + composer
   * direct-target selector. No longer takes the thread's Team id — the roster is
   * scoped server-side from the project, so an admin viewing another squad's
   * project sees that squad's agents instead of an empty rail. Optional — absent
   * degrades to a roster-less room (party-only composer).
   */
  loadRoster?: () => Promise<RosterAgent[]>;
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
  // messageId → joined proposal card (list-proposals). The transcript stays
  // phase-less; this join is the durable card state after a reload (ISI-4930).
  const [proposals, setProposals] = useState<Record<string, Proposal>>({});
  // Message id whose confirm/dismiss round-trip is in flight.
  const [busyMessageId, setBusyMessageId] = useState<string>();
  // Dispatch working-state (ISI-5174): the live "an agent is working…" entries,
  // seeded when the human posts an @-mention and resolved by the agent's reply
  // landing over the SAME SSE channel (no parallel status stream, AC4).
  const [working, setWorking] = useState<DispatchWatch[]>([]);
  // Live run thinking (ISI-5208): messageId → the streamed thinking envelopes for
  // the run(s) that message dispatched, rendered inline beneath it while the run is
  // active. Seeded from the `thinking` events arriving over the SAME project channel
  // and correlated to a working watch by author (lib/discussion/liveThinking.ts).
  const [liveThinking, setLiveThinking] = useState<
    Record<string, ThinkingRow[]>
  >({});
  // The roster the post callback reads to resolve dispatch targets, held in a
  // ref so `post` is not re-created on every roster refresh (and never dispatches
  // against a stale closure).
  const rosterRef = useRef<RosterAgent[]>([]);
  rosterRef.current = rosterAgents;
  // Current working watches, held in a ref so the stable subscribe closure
  // correlates an incoming thinking envelope against the LIVE watch set (not the
  // set captured when the subscription was opened).
  const workingRef = useRef<DispatchWatch[]>([]);
  workingRef.current = working;

  const load = useCallback(async () => {
    try {
      const flat = await client.getThread(projectId, threadId);
      setMessages(flat);
      setState("ready");
      // Proposal cards + lifecycle phase (story 6 read side). A failure here
      // degrades silently to plain transcript rendering — the room already
      // rendered, and the cards rejoin on the next load.
      try {
        const list = await client.listProposals(projectId, threadId);
        const next: Record<string, Proposal> = {};
        for (const p of list) next[p.Message.id] = p;
        setProposals(next);
      } catch {
        setProposals({});
      }
      // The project scopes the roster (§4.5; ISI-5107 — no longer the caller's
      // team via thread.teamId, which rendered an empty rail for admins viewing
      // another squad's project). A roster failure degrades silently to a
      // roster-less room — the thread itself already rendered.
      try {
        if (loadRoster) {
          const agents = await loadRoster();
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

  // Live append over the single 8.2 SSE channel (idempotent by id). An agent's
  // reply arriving here also resolves any working watch it answers (ISI-5174):
  // the affordance rides the same bus, not a parallel status stream (AC4).
  useEffect(() => {
    if (!subscribe) return;
    const unsub = subscribe((se) => {
      if (se.kind === "thinking") {
        // Live run thinking: correlate to the working watch it belongs under and
        // append it inline (dropped if it matches no active dispatch in this thread).
        setLiveThinking((cur) =>
          appendThinking(cur, se.row, workingRef.current),
        );
        return;
      }
      const evt = se.event;
      setMessages((cur) => applyRoomEvent(cur, evt));
      if (evt.type === "message.created" || evt.type === "message.updated") {
        // An agent reply landing over the SAME bus resolves its working watch to
        // "replied" (ISI-5174) — no parallel status stream (AC4).
        setWorking((cur) => applyReply(cur, evt.message));
      } else if (evt.type === "dispatch.failed") {
        // A discussion run ended without a reply (failed/cancelled): flip its
        // working watch → "failed" now instead of waiting for the client timeout
        // (ISI-5272). Correlated by agent name (+ messageId when present).
        setWorking((cur) =>
          applyDispatchFailed(cur, {
            agentName: evt.agentName,
            messageId: evt.messageId,
          }),
        );
      }
    });
    return unsub;
  }, [subscribe]);

  // Failure sweep (ISI-5174 AC3): expire a still-working watch to "could not
  // respond" once its run has been silent past the timeout, so an indicator
  // never spins forever. The reducer is pure in `now`; this timer is the clock.
  useEffect(() => {
    if (!hasActiveWork(working)) return;
    const t = setInterval(() => {
      setWorking((cur) => expireStale(cur, Date.now(), WORKING_TIMEOUT_MS));
    }, WORKING_SWEEP_MS);
    return () => clearInterval(t);
  }, [working]);

  const threads = useMemo(() => nestMessages(messages), [messages]);
  const workingByMessageId = useMemo(() => groupByMessage(working), [working]);

  const post = useCallback(
    async (body: { body: string; parentId?: string; audience?: string }) => {
      const created = await client.postMessage(projectId, threadId, body);
      // Optimistic upsert; the SSE echo is deduped by id.
      setMessages((cur) =>
        applyRoomEvent(cur, { type: "message.created", message: created }),
      );
      // ISI-5174: seed the "an agent is working…" affordance for each agent this
      // post @-mentions — resolved server-side the same way (dispatch.go). The
      // wire audience token (party vs direct:{id}) decides the fan-out, matching
      // the backend; a post that dispatches nobody seeds nothing.
      const agentNames = dispatchTargets(
        body.body,
        audienceWire(body.audience),
        rosterRef.current,
      );
      if (agentNames.length > 0) {
        setWorking((cur) =>
          seedWatches(cur, {
            messageId: created.id,
            agentNames,
            now: Date.now(),
          }),
        );
      }
    },
    [client, projectId, threadId],
  );

  // Human confirm (plan §4.4): fan into the authoring seams, then advance the
  // card to executed and append the result post-back message under it.
  const confirmProposal = useCallback(
    async (messageId: string) => {
      setBusyMessageId(messageId);
      try {
        const res = await client.confirmProposal(projectId, messageId);
        setProposals((cur) => {
          const existing = cur[messageId];
          if (!existing) return cur;
          return { ...cur, [messageId]: { ...existing, phase: res.status } };
        });
        const postBack = res.postBack;
        if (postBack) {
          setMessages((cur) =>
            applyRoomEvent(cur, {
              type: "message.created",
              message: postBack,
            }),
          );
        }
      } finally {
        setBusyMessageId(undefined);
      }
    },
    [client, projectId],
  );

  // Human dismiss (plan §4.4): record the decision, no fan-out.
  const dismissProposal = useCallback(
    async (messageId: string) => {
      setBusyMessageId(messageId);
      try {
        await client.dismissProposal(projectId, messageId);
        setProposals((cur) => {
          const existing = cur[messageId];
          if (!existing) return cur;
          return { ...cur, [messageId]: { ...existing, phase: "dismissed" } };
        });
      } finally {
        setBusyMessageId(undefined);
      }
    },
    [client, projectId],
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
            <MessageItem
              key={t.id}
              message={t}
              projectId={projectId}
              proposalByMessageId={proposals}
              onConfirmProposal={confirmProposal}
              onDismissProposal={dismissProposal}
              busyMessageId={busyMessageId}
              workingByMessageId={workingByMessageId}
              thinkingByMessageId={liveThinking}
            />
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
