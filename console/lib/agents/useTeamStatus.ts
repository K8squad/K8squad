"use client";

// lib/agents/useTeamStatus.ts — live agent-status client for the org diagram (story 8.10).
//
// Rides the SAME one-EventSource-against-the-BFF discipline as lib/useRunStream (§13 "one bus, no
// polling"). One native EventSource points at the BFF status stream (`/api/teams/{id}/status/stream`),
// never the Go apiserver — the browser auto-reconnects with Last-Event-ID and the apiserver replays
// the durable tail; the client implements no retry loop. Read-only: exposes a status map for
// rendering only; no mutate/claim/kill affordance rides the stream (R6 scope guard).
//
// Each SSE frame is a per-agent status delta `{ agentId, status, pausedReason?, currentRunId? }`;
// the hook folds deltas into a live `{ [agentId]: AgentStatusDelta }` map the diagram overlays on
// the initial server-rendered org snapshot, so a phase change repaints a single node without a
// refetch.

import { useEffect, useRef, useState } from "react";
import type { AgentStatus } from "./types";

export type AgentStatusDelta = {
  agentId: string;
  status: AgentStatus;
  pausedReason?: "credential" | "rate_limited" | null;
  currentRunId?: string | null;
};

export type StreamStatus = "connecting" | "open" | "error";

// A native EventSource auto-reconnects on a transient drop (ingress/gateway recycling the
// text/event-stream connection, a brief 5xx, a node roll). Escalating to a persistent alarming
// "error" on the FIRST onerror makes the chip flap live↔error every recycle even though the stream
// self-heals within a second or two (ISI-4737 symptom A). We instead hold the last-good state
// through a grace window and only surface "error" if the connection stays down past it — a brief
// reconnect never reaches the user. Long enough to cover an ingress recycle + reconnect handshake,
// short enough that a genuine sustained outage still surfaces promptly.
export const RECONNECT_GRACE_MS = 10_000;

const STATUSES: ReadonlySet<string> = new Set([
  "idle",
  "running",
  "blocked",
  "paused",
]);

function coerceDelta(raw: unknown): AgentStatusDelta | null {
  if (typeof raw !== "object" || raw === null) return null;
  const r = raw as Record<string, unknown>;
  const agentId = typeof r.agentId === "string" ? r.agentId : "";
  const status = typeof r.status === "string" ? r.status : "";
  if (!agentId || !STATUSES.has(status)) return null; // render only known, server-stamped deltas
  const pausedReason =
    r.pausedReason === "credential" || r.pausedReason === "rate_limited"
      ? r.pausedReason
      : null;
  return {
    agentId,
    status: status as AgentStatus,
    pausedReason,
    currentRunId: typeof r.currentRunId === "string" ? r.currentRunId : null,
  };
}

export function useTeamStatus(teamId: string) {
  const [deltas, setDeltas] = useState<Record<string, AgentStatusDelta>>({});
  const [status, setStatus] = useState<StreamStatus>("connecting");
  const esRef = useRef<EventSource | null>(null);
  const graceTimer = useRef<ReturnType<typeof setTimeout> | null>(null);

  useEffect(() => {
    if (!teamId) return;
    const clearGrace = () => {
      if (graceTimer.current !== null) {
        clearTimeout(graceTimer.current);
        graceTimer.current = null;
      }
    };
    const es = new EventSource(
      `/api/teams/${encodeURIComponent(teamId)}/status/stream`,
    );
    esRef.current = es;

    es.onopen = () => {
      clearGrace(); // a (re)connect landed — cancel any pending error escalation
      setStatus("open");
    };
    es.onerror = () => {
      // readyState CLOSED ⇒ the browser will NOT auto-reconnect (a fatal 4xx / CORS):
      // that is a genuine terminal error, surface it at once. CONNECTING ⇒ a transient
      // drop the browser is already re-establishing — hold the last-good state through
      // the grace window and only escalate to "error" if it stays down (ISI-4737 §A / AC-A).
      if (es.readyState === EventSource.CLOSED) {
        clearGrace();
        setStatus("error");
        return;
      }
      if (graceTimer.current === null) {
        graceTimer.current = setTimeout(() => {
          graceTimer.current = null;
          setStatus("error");
        }, RECONNECT_GRACE_MS);
      }
    };
    es.onmessage = (msg: MessageEvent) => {
      let parsed: unknown;
      try {
        parsed = JSON.parse(msg.data);
      } catch {
        return; // ignore non-JSON keepalives
      }
      const d = coerceDelta(parsed);
      if (d) setDeltas((prev) => ({ ...prev, [d.agentId]: d }));
    };

    return () => {
      clearGrace();
      es.close();
      esRef.current = null;
    };
  }, [teamId]);

  return { deltas, status };
}
