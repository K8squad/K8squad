"use client";

// Client wrapper for the Discussion route: resolves the Project's room (R1 — the
// room IS the Project, keyed by projectId) as its thread list, picks the active
// thread, and mounts the collapsible <DiscussionsPanel> (ISI-5673) beside
// <DiscussionRoom> with a BFF-backed client and the shared 8.2 SSE
// subscription. Kept separate from page.tsx so the server component stays thin.
// There is no backend room object or `roomId`; the thread id is the sub-resource
// id (migration 0004 superseded the naive rooms shape).
//
// ISI-5673 (ISI-5598 WS-C): the active thread is driven by a `?thread={id}` URL
// query param (linkable / back-button friendly) instead of the old hardcoded
// `threads[0]` pick, and the full thread list feeds the left Discussions panel
// (open-existing selects a row → `?thread`; new-chat POSTs a thread and selects
// it). This is UI-surfacing only — the thread list/open APIs already exist.
//
// ISI-4929: also wires the roster loader (Team org read model → the room's
// roster/presence sidebar + the composer's direct-target selector) and the
// mention search (ISI-4926 endpoint) into the room. Both are optional props on
// <DiscussionRoom>; failures degrade to a roster-less, popover-less composer.

import { useCallback, useEffect, useMemo, useState } from "react";
import { usePathname, useRouter, useSearchParams } from "next/navigation";
import { createDiscussionClient, type DiscussionClient } from "@/lib/discussion/api";
import { subscribeRoom, type EventSourceFactory } from "@/lib/discussion/sse";
import type { RoomStreamEvent } from "@/lib/discussion/liveFeed";
import type { RosterAgent } from "@/components/discussion/Roster";
import { DiscussionRoom } from "@/components/discussion/DiscussionRoom";
import { DiscussionsPanel, type PanelListState } from "@/components/discussion/DiscussionsPanel";
import type { Thread } from "@/lib/discussion/types";
import {
  resolveActiveThreadId,
  sortThreadsByRecency,
  upsertThread,
} from "@/lib/discussion/threadList";
import type { SquadOverviewData } from "@/components/SquadOverview";
import { liveRunsByAgent, type AgentRunPresence } from "@/lib/overview/liveRuns";

// R1 empty-room bootstrap: a fresh Project has no threads, and nothing else in
// the UI can open the first one — without this the route sat on "Loading…"
// forever. The room IS the Project, so the first visitor auto-opens the default
// General thread.
const DEFAULT_THREAD = {
  title: "General",
  body: "Project discussion room — agents and humans, threaded. Post here to reach the team; @-mention an agent or ticket to pull them in.",
} as const;

// Backend `OpenThread` creates thread + first message atomically and rejects an
// empty body (ErrEmptyBody) — there is no rename/empty-thread endpoint. A
// user-created chat therefore opens with this neutral opener as its first
// message; the human's own posts follow in the composer.
const NEW_CHAT_OPENER_BODY = "New discussion.";

// Live-run presence refresh cadence (ISI-5527). Reuse the ONE live-runs source this console has —
// GET /api/squad/overview — the SAME feed the nav badge (ISI-5526) and the Overview surfaces poll;
// no new endpoint, no new schema. 4s is a fine presence cue (the task-list pill polls at 3s
// upstream). A failed read is cosmetic: the roster keeps its last good state and degrades to idle.
const OVERVIEW_POLL_MS = 4000;

const defaultLoadOverview = () =>
  fetch("/api/squad/overview", {
    headers: { accept: "application/json" },
    cache: "no-store",
  });

export function DiscussionRoomClient({
  projectId,
  loadOverview = defaultLoadOverview,
  client: clientProp,
}: {
  projectId: string;
  /** Injectable for tests; defaults to the BFF GET /api/squad/overview fetch. */
  loadOverview?: () => Promise<Response>;
  /** Injectable for tests; defaults to the BFF-backed discussion client. */
  client?: DiscussionClient;
}) {
  const defaultClient = useMemo(() => createDiscussionClient(), []);
  const client = clientProp ?? defaultClient;
  const router = useRouter();
  const pathname = usePathname();
  const searchParams = useSearchParams();
  const requestedThreadId = searchParams.get("thread");

  const [threads, setThreads] = useState<Thread[]>([]);
  const [listState, setListState] = useState<PanelListState>("loading");
  const [attempt, setAttempt] = useState(0);
  // Per-agent live-run presence for THIS project's room (keyed by roster agent id), folded from the
  // squad/overview feed. Empty = every agent idle; drives the roster's idle⇄running flip (ISI-5527).
  const [liveRuns, setLiveRuns] = useState<Record<string, AgentRunPresence>>(
    {},
  );

  // The active thread is a pure derivation of the URL request + the loaded list
  // (ISI-5673): a valid `?thread={id}` wins; otherwise the newest thread; null
  // only while the list is empty (the bootstrap below then opens one).
  const activeThreadId = useMemo(
    () => resolveActiveThreadId(threads, requestedThreadId),
    [threads, requestedThreadId],
  );

  // Load the Project's thread list (and, for a fresh Project, bootstrap the
  // default thread). Lifted out of the per-thread room so the panel always has
  // the full list and switching threads never re-lists.
  useEffect(() => {
    let alive = true;
    setListState("loading");
    void (async () => {
      try {
        let list = await client.listThreads(projectId);
        if (!alive) return;
        if (list.length === 0) {
          try {
            const opened = await client.openThread(projectId, DEFAULT_THREAD);
            list = [opened];
          } catch {
            // A racing first visitor may have opened the default thread just
            // ahead of us — re-list and adopt it before giving up.
            const retry = await client.listThreads(projectId);
            if (!alive) return;
            list = retry;
          }
        }
        if (!alive) return;
        if (list.length === 0) {
          setListState("error");
          return;
        }
        setThreads(sortThreadsByRecency(list));
        setListState("ready");
      } catch {
        if (alive) setListState("error");
      }
    })();
    return () => {
      alive = false;
    };
  }, [client, projectId, attempt]);

  // Canonicalize the URL: once the list is ready, if the request was absent or
  // stale, pin `?thread={active}` so the surface is always linkable (replace —
  // no history entry; explicit row selection uses push for back-button nav).
  useEffect(() => {
    if (listState !== "ready" || !activeThreadId) return;
    if (requestedThreadId === activeThreadId) return;
    const params = new URLSearchParams(searchParams.toString());
    params.set("thread", activeThreadId);
    router.replace(`${pathname}?${params.toString()}`, { scroll: false });
  }, [
    listState,
    activeThreadId,
    requestedThreadId,
    pathname,
    router,
    searchParams,
  ]);

  const selectThread = useCallback(
    (id: string) => {
      const params = new URLSearchParams(searchParams.toString());
      params.set("thread", id);
      router.push(`${pathname}?${params.toString()}`, { scroll: false });
    },
    [pathname, router, searchParams],
  );

  const createChat = useCallback(
    async (title: string) => {
      try {
        const opened = await client.openThread(projectId, {
          title,
          body: NEW_CHAT_OPENER_BODY,
        });
        setThreads((cur) => upsertThread(cur, opened));
        const params = new URLSearchParams(searchParams.toString());
        params.set("thread", opened.id);
        router.push(`${pathname}?${params.toString()}`, { scroll: false });
      } catch {
        // Surfacing the failure inline keeps the panel honest; the list read is
        // untouched, so a retry (or a repeat New-chat) is always available.
        setListState("error");
      }
    },
    [client, projectId, pathname, router, searchParams],
  );

  const retry = useCallback(() => setAttempt((n) => n + 1), []);

  const subscribe = useMemo(() => {
    if (typeof EventSource === "undefined" || activeThreadId == null)
      return undefined;
    const factory: EventSourceFactory = (url) =>
      new EventSource(url) as unknown as ReturnType<EventSourceFactory>;
    return (onEvent: (evt: RoomStreamEvent) => void) =>
      subscribeRoom(projectId, activeThreadId, onEvent, factory);
  }, [projectId, activeThreadId]);

  // Roster: the project-scoped roster read (ISI-5107) — the agents dispatchable
  // into THIS project, resolved server-side from the path (admin ⇒ the project
  // namespace, everyone else ⇒ their own team) rather than from the caller's team
  // via thread.teamId, which rendered an empty rail for admins on another squad's
  // project. A read failure degrades the room to roster-less.
  const loadRoster = useCallback(
    (): Promise<RosterAgent[]> =>
      client.getRoster(projectId).then((agents) =>
        agents.map((a) => ({
          id: a.id,
          name: a.name,
          status: a.status,
        })),
      ),
    [client, projectId],
  );

  // Live-run presence (ISI-5527): once the room is live, poll the squad/overview feed and fold
  // this project's live runs into a per-agent map. Reuse-only — the same endpoint the nav badge
  // and Overview surfaces read, no new backend. A failed/absent read is cosmetic: the last good
  // map stays in place, and an unwired overview simply leaves every row idle.
  useEffect(() => {
    if (activeThreadId == null) return;
    let alive = true;
    const refresh = () => {
      loadOverview()
        .then((res) => (res.ok ? res.json() : null))
        .then((body: SquadOverviewData | null) => {
          if (alive) setLiveRuns(liveRunsByAgent(body, projectId));
        })
        .catch(() => {
          // Cosmetic signal — swallow and keep the last good presence map.
        });
    };
    refresh();
    const timer = setInterval(refresh, OVERVIEW_POLL_MS);
    return () => {
      alive = false;
      clearInterval(timer);
    };
  }, [activeThreadId, projectId, loadOverview]);

  const searchMentions = useCallback(
    (q: string) => client.searchMentions(projectId, q),
    [client, projectId],
  );

  return (
    <div className="ksq-roomlayout" data-testid="discussion-room-layout">
      <DiscussionsPanel
        threads={threads}
        activeThreadId={activeThreadId}
        listState={listState}
        onSelect={selectThread}
        onNewChat={createChat}
        onRetry={retry}
      />
      <div className="ksq-roomlayout__main">
        {activeThreadId == null ? (
          listState === "error" ? (
            <div data-testid="room-resolve-error" className="muted">
              <p>Could not open the discussion room.</p>
              <button type="button" onClick={retry}>
                Retry
              </button>
            </div>
          ) : (
            <div data-testid="room-resolving">Loading…</div>
          )
        ) : (
          <DiscussionRoom
            key={activeThreadId}
            projectId={projectId}
            threadId={activeThreadId}
            client={client}
            subscribe={subscribe}
            loadRoster={loadRoster}
            searchMentions={searchMentions}
            liveRuns={liveRuns}
          />
        )}
      </div>
    </div>
  );
}
