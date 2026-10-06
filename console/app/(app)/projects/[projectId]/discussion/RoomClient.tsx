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
import type { RoomStreamEvent } from "@/lib/discussion/liveFeed";
import type { RosterAgent } from "@/components/discussion/Roster";
import { DiscussionRoom } from "@/components/discussion/DiscussionRoom";
import type { SquadOverviewData } from "@/components/SquadOverview";
import { liveRunsByAgent, type AgentRunPresence } from "@/lib/overview/liveRuns";

// R1 empty-room bootstrap: a fresh Project has no threads, and nothing else in
// the UI can open the first one (the composer only mounts once a threadId
// resolves) — without this the route sat on "Loading…" forever. The room IS
// the Project, so the first visitor auto-opens the default General thread.
const DEFAULT_THREAD = {
  title: "General",
  body: "Project discussion room — agents and humans, threaded. Post here to reach the team; @-mention an agent or ticket to pull them in.",
} as const;

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
}: {
  projectId: string;
  /** Injectable for tests; defaults to the BFF GET /api/squad/overview fetch. */
  loadOverview?: () => Promise<Response>;
}) {
  const client = useMemo(() => createDiscussionClient(), []);
  const [threadId, setThreadId] = useState<string | null>(null);
  const [failed, setFailed] = useState(false);
  const [attempt, setAttempt] = useState(0);
  // Per-agent live-run presence for THIS project's room (keyed by roster agent id), folded from the
  // squad/overview feed. Empty = every agent idle; drives the roster's idle⇄running flip (ISI-5527).
  const [liveRuns, setLiveRuns] = useState<Record<string, AgentRunPresence>>(
    {},
  );

  useEffect(() => {
    let alive = true;
    void (async () => {
      try {
        const threads = await client.listThreads(projectId);
        if (!alive) return;
        if (threads.length > 0) {
          setThreadId(threads[0].id);
          setFailed(false);
          return;
        }
        try {
          const opened = await client.openThread(projectId, DEFAULT_THREAD);
          if (alive) {
            setThreadId(opened.id);
            setFailed(false);
          }
        } catch {
          // A racing first visitor may have opened the default thread just
          // ahead of us — re-list and adopt it before giving up.
          const retry = await client.listThreads(projectId);
          if (!alive) return;
          if (retry.length > 0) {
            setThreadId(retry[0].id);
            setFailed(false);
          } else setFailed(true);
        }
      } catch {
        if (alive) setFailed(true);
      }
    })();
    return () => {
      alive = false;
    };
  }, [client, projectId, attempt]);

  const subscribe = useMemo(() => {
    if (typeof EventSource === "undefined" || threadId == null) return undefined;
    const factory: EventSourceFactory = (url) =>
      new EventSource(url) as unknown as ReturnType<EventSourceFactory>;
    return (onEvent: (evt: RoomStreamEvent) => void) =>
      subscribeRoom(projectId, threadId, onEvent, factory);
  }, [projectId, threadId]);

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
    if (threadId == null) return;
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
  }, [threadId, projectId, loadOverview]);

  const searchMentions = useCallback(
    (q: string) => client.searchMentions(projectId, q),
    [client, projectId],
  );

  if (threadId == null) {
    if (failed) {
      return (
        <div data-testid="room-resolve-error" className="muted">
          <p>Could not open the discussion room.</p>
          <button type="button" onClick={() => setAttempt((n) => n + 1)}>
            Retry
          </button>
        </div>
      );
    }
    return <div data-testid="room-resolving">Loading…</div>;
  }

  return (
    <DiscussionRoom
      projectId={projectId}
      threadId={threadId}
      client={client}
      subscribe={subscribe}
      loadRoster={loadRoster}
      searchMentions={searchMentions}
      liveRuns={liveRuns}
    />
  );
}
