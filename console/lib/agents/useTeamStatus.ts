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

  useEffect(() => {
    if (!teamId) return;
    const es = new EventSource(
      `/api/teams/${encodeURIComponent(teamId)}/status/stream`,
    );
    esRef.current = es;

    es.onopen = () => setStatus("open");
    es.onerror = () => setStatus("error"); // native EventSource auto-reconnects w/ Last-Event-ID
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
      es.close();
      esRef.current = null;
    };
  }, [teamId]);

  return { deltas, status };
}
