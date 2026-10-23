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
import type {
  MentionSuggestion,
  Message,
  PartySession,
  Proposal,
} from "@/lib/discussion/types";
import { nestMessages } from "@/lib/discussion/thread";
import { isPartyStart, newestTerminalSession } from "@/lib/discussion/party";
import {
  applyRoomEvent,
  isResetMarker,
  type RoomStreamEvent,
  type ThinkingRow,
} from "@/lib/discussion/liveFeed";
import { appendThinking } from "@/lib/discussion/liveThinking";
import { audienceWire } from "@/lib/discussion/audience";
import {
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
import { PartySessionCard } from "./PartySessionCard";
import { Composer } from "./Composer";
import { ResetConfirmDialog } from "./ResetConfirmDialog";
import { Roster, type RosterAgent } from "./Roster";
import type { AgentRunPresence } from "@/lib/overview/liveRuns";
import "./discussion.css";

// How long a dispatched agent may stay silent before the room shows "could not
// respond" (ISI-5174 AC3). A discussion thread-run reads the thread, runs a
// model turn, and posts back — minutes, not seconds — so this is generous enough
// not to false-fail a live run yet short enough that a dead run stops spinning.
const WORKING_TIMEOUT_MS = 5 * 60_000;
// Cadence at which the room re-checks working watches against the clock. The
// reducer (expireStale) is pure in `now`; this timer just supplies the tick.
const WORKING_SWEEP_MS = 15_000;
// Cadence at which the room re-polls the live party session (ISI-5589 WS-E).
// WS-B pushes NO session-state mutation over SSE (round advance / close), so the
// round + budget meter is refreshed by re-GETting `/party-sessions/active` while
// a party_start opener is in the thread. Only runs when a debate is present.
const PARTY_POLL_MS = 20_000;

// Human-readable timestamp for the post-reset "reset by <who> · <time>" header
// (ISI-5674 WS-D). Degrades to the raw ISO on an unparseable value so the header
// never renders "Invalid Date".
function formatResetTime(iso: string): string {
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleString();
}

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
  /**
   * Per-agent live-run presence (ISI-5527, keyed by roster agent id), derived by
   * RoomClient from the polling squad/overview feed. Drives the roster's idle ⇄
   * queued ⇄ running flip. Optional — absent degrades the roster to plain presence.
   */
  liveRuns?: Record<string, AgentRunPresence>;
}

type LoadState = "loading" | "ready" | "not-found" | "error";

export function DiscussionRoom({
  projectId,
  threadId,
  client,
  subscribe,
  loadRoster,
  searchMentions,
  liveRuns,
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
  // The thread's live party session (ISI-5585 WS-B), fetched from
  // `/party-sessions/active`. null ⇒ no active debate.
  const [partySession, setPartySession] = useState<PartySession | null>(null);
  // The thread's TERMINAL party session (ISI-5617, ISI-5613 Gap 2), fetched from
  // the `?includeClosed=true` list once the live session is gone. It lets the
  // post-close takeaways carry the real phase/round/paid-run numbers instead of
  // the transcript-only degrade; null until a closed debate is read (or on a
  // pre-ISI-5617 apiserver, where the card falls back to the transcript).
  const [terminalSession, setTerminalSession] = useState<PartySession | null>(
    null,
  );
  // Reset state (ISI-5674 WS-D, ADR-0030). `resetInfo` is the latest live reset
  // marker's metadata (who/when) — set from the thread read on load and from the
  // marker when a reset lands (POST response or SSE). It drives the post-reset
  // empty state. `resetOpen` toggles the confirm dialog; `resetBusy` guards the
  // in-flight reset round-trip against a double-run.
  const [resetInfo, setResetInfo] = useState<{ at: string; by: string } | null>(
    null,
  );
  const [resetOpen, setResetOpen] = useState(false);
  const [resetBusy, setResetBusy] = useState(false);
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
      // Reset metadata (ISI-5674 WS-D, ADR-0030 §(c)): read the thread record so a
      // thread that was reset shows its post-reset empty state after a reload —
      // the floored get-thread returns no bubbles, and `resetAt`/`resetBy` carry
      // the "reset by <who> · <time>" header. Best-effort: on any failure (or a
      // pre-ISI-5672 apiserver that omits the fields) the room just renders the
      // plain empty transcript.
      try {
        if (typeof client.getThreadInfo === "function") {
          const info = await client.getThreadInfo(projectId, threadId);
          setResetInfo(
            info.resetAt && info.resetBy
              ? { at: info.resetAt, by: info.resetBy }
              : null,
          );
        }
      } catch {
        setResetInfo(null);
      }
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
      // Live party session (ISI-5589 WS-E): the round + budget meter source.
      // Optional on the client (older stubs omit it) and best-effort — a failure
      // degrades to no party framing, the thread already rendered.
      let activeParty: PartySession | null = null;
      try {
        if (typeof client.getActivePartySession === "function") {
          activeParty = await client.getActivePartySession(projectId, threadId);
          setPartySession(activeParty);
        }
      } catch {
        setPartySession(null);
      }
      // Post-close takeaways (ISI-5617, ISI-5613 Gap 2): with no LIVE session but
      // a party_start opener in the transcript, the debate has closed — read the
      // terminal session from the include-closed list so the takeaways carry the
      // real phase/round/paid-run numbers. Best-effort: a pre-ISI-5617 apiserver
      // (no list method, or it throws) leaves terminalSession null and the card
      // falls back to the transcript-only takeaways.
      try {
        if (
          !activeParty &&
          flat.some((m) => isPartyStart(m)) &&
          typeof client.listPartySessions === "function"
        ) {
          const list = await client.listPartySessions(
            projectId,
            threadId,
            true,
          );
          setTerminalSession(newestTerminalSession(list));
        } else {
          setTerminalSession(null);
        }
      } catch {
        setTerminalSession(null);
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
      // ADR-0030 clear-signal (ISI-5674 WS-D): a reset marker over the bus clears
      // the local transcript (applyRoomEvent drops the floor, never a bubble) and
      // flips the room into its post-reset empty state. Record who/when for the
      // "reset by <who> · <time>" header.
      if (
        (evt.type === "message.created" || evt.type === "message.updated") &&
        isResetMarker(evt.message)
      ) {
        setResetInfo({
          at: evt.message.createdAt,
          by: evt.message.authorPrincipal,
        });
      }
      setMessages((cur) => applyRoomEvent(cur, evt));
      if (evt.type === "message.created" || evt.type === "message.updated") {
        // An agent reply landing over the SAME bus resolves its working watch to
        // "replied" (ISI-5174) — no parallel status stream (AC4).
        setWorking((cur) => applyReply(cur, evt.message));
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
  // The party_start opener in the transcript, if any (ISI-5589 WS-E). Its
  // presence means this thread hosts a party debate — the framing/poll anchor,
  // and the fallback source for the takeaways card once the session has closed.
  const partyOpener = useMemo(
    () => messages.find((m) => isPartyStart(m)) ?? null,
    [messages],
  );

  // Re-poll the live party session while a debate is present (ISI-5589 WS-E).
  // WS-B emits no session-state SSE, so the meter is refreshed by re-GET. The
  // poll stops when the thread has no party_start opener (no debate to track) or
  // once a terminal session has been read (ISI-5613 Gap 2) — a closed debate has
  // no more live state to track, so polling would just re-null `partySession`.
  useEffect(() => {
    if (!partyOpener) return;
    if (terminalSession) return;
    if (typeof client.getActivePartySession !== "function") return;
    let cancelled = false;
    const t = setInterval(() => {
      void client
        .getActivePartySession(projectId, threadId)
        .then((ps) => {
          if (cancelled) return;
          setPartySession(ps);
          // The debate just closed: fetch its terminal session so the takeaways
          // keep the real phase/round/paid-run numbers (ISI-5617, Gap 2).
          if (!ps && typeof client.listPartySessions === "function") {
            void client
              .listPartySessions(projectId, threadId, true)
              .then((list) => {
                if (!cancelled) setTerminalSession(newestTerminalSession(list));
              })
              .catch(() => {
                /* leave terminalSession null → transcript-only fallback */
              });
          }
        })
        .catch(() => {
          /* keep the last-seen session on a transient poll failure */
        });
    }, PARTY_POLL_MS);
    return () => {
      cancelled = true;
      clearInterval(t);
    };
  }, [partyOpener, terminalSession, client, projectId, threadId]);

  const post = useCallback(
    async (body: { body: string; parentId?: string; audience?: string }) => {
      const created = await client.postMessage(projectId, threadId, body);
      // Optimistic upsert; the SSE echo is deduped by id.
      setMessages((cur) =>
        applyRoomEvent(cur, { type: "message.created", message: created }),
      );
      // ISI-5174: seed the "an agent is working…" affordance for each agent this
      // post dispatches — resolved server-side the same way (dispatch.go). The
      // wire audience token (party vs direct:{id}) and authorship decide the
      // fan-out, matching the backend: a human bare-party post broadcasts to the
      // whole room (ISI-5265), a direct/@-mention post targets its agents, and a
      // post that dispatches nobody seeds nothing.
      const agentNames = dispatchTargets(
        body.body,
        audienceWire(body.audience),
        rosterRef.current,
        { authoredByAgent: Boolean(created.authorAgentId) },
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

  // Human reset (ISI-5674 WS-D, ADR-0030): run the WS-B endpoint, then apply the
  // returned marker locally as the clear-signal — drop the transcript (never a
  // bubble) and flip into the post-reset empty state. The SSE echo of the same
  // marker is idempotent by id, so a double-apply is a no-op. The thread id is
  // unchanged (`?thread` stays stable). A failure leaves the dialog open with the
  // transcript intact; the busy guard prevents a double-run.
  const doReset = useCallback(async () => {
    setResetBusy(true);
    try {
      const res = await client.resetThread(projectId, threadId);
      setMessages((cur) =>
        applyRoomEvent(cur, { type: "message.created", message: res.marker }),
      );
      setResetInfo({
        at: res.marker.createdAt,
        by: res.marker.authorPrincipal,
      });
      setResetOpen(false);
    } finally {
      setResetBusy(false);
    }
  }, [client, projectId, threadId]);

  // Composer slash command (ISI-5674 WS-D): `/reset` opens the confirm dialog.
  // `/reset` is the only command this phase, so an unknown name is ignored.
  const onCommand = useCallback((name: string) => {
    if (name === "reset") setResetOpen(true);
  }, []);

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

  // ISI-5741 (parent ISI-5737) — sticky 3-panel room. The ROOM VIEWPORT, not the
  // page, is now the bounded surface: the roster rail, the history column, and
  // the composer dock are the three regions that stay visible at any scroll
  // position. Only the history column scrolls internally (`.ksq-room__history`,
  // discussion.css), so the scroll owner moved OFF the document and onto that
  // container — the inner-transcript-scroll retarget the ISI-5597/5623 note had
  // deferred. This supersedes the earlier "page is the scroll owner" decision for
  // the discussion room only (ticket-detail is untouched).
  //
  // Two small effects track the internal scroll container: (1) keep the dock's
  // live height in `--ksq-dock-h` so the FAB rides just above it (the composer
  // grows on error / when the @-mention popover opens), and (2) drive the FAB's
  // visibility from how much transcript is still below the history fold.
  const dockRef = useRef<HTMLElement | null>(null);
  const historyRef = useRef<HTMLDivElement | null>(null);
  const [showJump, setShowJump] = useState(false);

  useEffect(() => {
    const el = dockRef.current;
    if (!el || typeof ResizeObserver === "undefined") return;
    const apply = () =>
      document.documentElement.style.setProperty(
        "--ksq-dock-h",
        `${el.offsetHeight}px`,
      );
    apply();
    const ro = new ResizeObserver(apply);
    ro.observe(el);
    return () => {
      ro.disconnect();
      document.documentElement.style.removeProperty("--ksq-dock-h");
    };
  }, [state]);

  // Show the FAB only while there is meaningful content below the fold of the
  // internal history scroll; it fades out within ~48px of the end, so it is never
  // a dead control at the bottom. New messages arriving (SSE) retrigger the
  // recompute via the messages.length dep, and a history resize (a growing bubble,
  // the dock reflowing) retriggers it via the ResizeObserver.
  useEffect(() => {
    if (state !== "ready") return;
    const el = historyRef.current;
    if (!el) return;
    const THRESHOLD = 48;
    const update = () => {
      const distance = el.scrollHeight - el.scrollTop - el.clientHeight;
      setShowJump(distance > THRESHOLD);
    };
    update();
    el.addEventListener("scroll", update, { passive: true });
    window.addEventListener("resize", update);
    let ro: ResizeObserver | undefined;
    if (typeof ResizeObserver !== "undefined") {
      ro = new ResizeObserver(update);
      ro.observe(el);
    }
    return () => {
      el.removeEventListener("scroll", update);
      window.removeEventListener("resize", update);
      ro?.disconnect();
    };
  }, [state, messages.length]);

  // Scroll the history column to the newest message (instant under reduced-motion).
  function jumpToBottom() {
    const el = historyRef.current;
    if (!el) return;
    const reduce =
      typeof window.matchMedia === "function" &&
      window.matchMedia("(prefers-reduced-motion: reduce)").matches;
    el.scrollTo({
      top: el.scrollHeight,
      behavior: reduce ? "auto" : "smooth",
    });
  }

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

  // Post-reset empty state (ISI-5674 WS-D, mocks §3 panel ⑤): a reset thread
  // reads with no bubbles (the floor) but keeps its reset metadata, so the room
  // shows the "This discussion was reset" empty state + "reset by <who> · <time>"
  // header instead of a blank transcript. It persists across a reload (resetInfo
  // is read off the thread) and until the human posts the first fresh message.
  const showResetEmptyState = messages.length === 0 && resetInfo !== null;

  return (
    <section className="ksq-room" data-testid="discussion-room">
      <div className="ksq-room__main">
        {/* ISI-5741 — the history column: the ONLY internally-scrolling region.
            The party card, reset empty state, and the transcript scroll within
            this bounded box; the dock + roster frames stay put (AC2/AC4). */}
        <div
          ref={historyRef}
          className="ksq-room__history"
          data-testid="room-history"
        >
          {partySession || terminalSession || partyOpener ? (
            <PartySessionCard
              session={partySession ?? terminalSession}
              opener={partyOpener}
              messages={messages}
            />
          ) : null}
          {showResetEmptyState ? (
            <div
              className="ksq-room__reset-empty"
              data-testid="room-reset-empty"
            >
              <p className="ksq-room__reset-meta" data-testid="room-reset-meta">
                reset by {resetInfo.by} · {formatResetTime(resetInfo.at)}
              </p>
              <div className="ksq-room__reset-illus" aria-hidden="true">
                <svg width="48" height="48" viewBox="0 0 24 24" fill="none">
                  <path
                    d="M4 12a8 8 0 1 1 2.3 5.6M4 12V7m0 5h5"
                    stroke="currentColor"
                    strokeWidth="1.6"
                    strokeLinecap="round"
                    strokeLinejoin="round"
                  />
                </svg>
              </div>
              <h2 className="ksq-room__reset-title">
                This discussion was reset
              </h2>
              <p className="ksq-room__reset-sub">
                Earlier messages are archived for audit and no longer shown
                here.
              </p>
            </div>
          ) : null}
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
        </div>
        {/* ISI-5741 (supersedes ISI-5623 P2) — the composer pinned to the bottom
            of the room as a flex sibling BELOW the scrolling history (no longer
            `position: sticky` inside the scroll area), so it is never scrolled
            off-screen (AC3). Same <Composer> — its @-mention + audience contract
            is untouched; only its placement (and the UPWARD popover) changed. */}
        <footer
          ref={dockRef}
          className="ksq-room__dock"
          data-testid="room-composer-dock"
        >
          <Composer
            onPost={post}
            onCommand={onCommand}
            placeholder={
              showResetEmptyState ? "Start the discussion…" : undefined
            }
            directTargets={rosterAgents.map((a) => ({
              id: a.id,
              name: a.name,
            }))}
            searchMentions={searchMentions}
          />
        </footer>

        {/* ISI-5741 (ISI-5597 P1, retargeted) — jump-to-bottom FAB, now pinned
            inside the center column (absolute, above the dock) rather than fixed
            to the viewport, since the history column — not the page — is the
            scroll owner. Shown only while transcript sits below the history fold;
            scrolls the history column to the newest message (instant under
            prefers-reduced-motion). A real focusable button, pulled from the tab
            order and hidden from AT while off-screen. */}
        <button
          type="button"
          className="ksq-jump-fab"
          data-testid="room-jump-bottom"
          data-visible={showJump}
          aria-label="Scroll to latest"
          aria-hidden={!showJump}
          tabIndex={showJump ? 0 : -1}
          onClick={jumpToBottom}
        >
          <svg
            width="20"
            height="20"
            viewBox="0 0 20 20"
            aria-hidden="true"
            focusable="false"
          >
            <path
              d="M10 3.5v11m0 0l4.5-4.5M10 14.5L5.5 10"
              fill="none"
              stroke="currentColor"
              strokeWidth="1.8"
              strokeLinecap="round"
              strokeLinejoin="round"
            />
          </svg>
        </button>
      </div>
      <Roster agents={rosterAgents} liveRuns={liveRuns} />

      {/* ISI-5674 WS-D — `/reset` confirm dialog (ADR-0030, mocks §3 ④). Focus-
          trapped, Cancel-default; on confirm it runs the WS-B endpoint and clears
          the transcript via the returned marker. Rendered at the room root so it
          overlays the whole surface. */}
      {resetOpen ? (
        <ResetConfirmDialog
          busy={resetBusy}
          onCancel={() => setResetOpen(false)}
          onConfirm={() => void doReset()}
        />
      ) : null}
    </section>
  );
}
