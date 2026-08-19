"use client";

// lib/useRunStream.ts — the ONE shared SSE client (story 8.2).
//
// Every live surface (Run stream, live-Run map 8.8f, org diagram 8.10, agent log tail 8.11,
// dashboard live tiles 8.8b/8.8c) rides THIS hook — one native EventSource against the BFF
// route, never a per-surface transport and NEVER a polling loop (§13 "one bus, no polling").
//
// The EventSource points at the BFF (`/api/runs/{runId}/stream`), never the Go apiserver (AC2).
// The browser's native EventSource auto-reconnects and sends Last-Event-ID; the apiserver
// replays the durable coord-record tail (AC5) — the client does not implement its own retry loop.
// Read-only: this hook exposes events for rendering only; no mutate/claim/kill affordance (AC6).

import { useEffect, useRef, useState } from "react";

/** Server-stamped coordination-event kinds (AC7). */
export type RunEventKind =
  "CHECKOUT" | "COMMENT" | "HANDOFF" | "MEMORY" | "ARTIFACT";

export type RunEvent = {
  id: string;
  kind: RunEventKind;
  actor: string; // agent·role, server-stamped
  ts: string; // server timestamp
  summary?: string;
};

export type StreamStatus = "connecting" | "open" | "error";

const KINDS: ReadonlySet<string> = new Set([
  "CHECKOUT",
  "COMMENT",
  "HANDOFF",
  "MEMORY",
  "ARTIFACT",
]);

function coerceEvent(id: string, raw: unknown): RunEvent | null {
  if (typeof raw !== "object" || raw === null) return null;
  const r = raw as Record<string, unknown>;
  const kind = typeof r.kind === "string" ? r.kind.toUpperCase() : "";
  if (!KINDS.has(kind)) return null; // render only server-stamped, known kinds (AC7)
  return {
    id,
    kind: kind as RunEventKind,
    actor: typeof r.actor === "string" ? r.actor : "unknown",
    ts: typeof r.ts === "string" ? r.ts : "",
    summary: typeof r.summary === "string" ? r.summary : undefined,
  };
}

export function useRunStream(runId: string) {
  const [events, setEvents] = useState<RunEvent[]>([]);
  const [status, setStatus] = useState<StreamStatus>("connecting");
  const esRef = useRef<EventSource | null>(null);

  useEffect(() => {
    if (!runId) return;
    // ONE EventSource, against the BFF — never the apiserver directly (AC2). No polling.
    const es = new EventSource(`/api/runs/${encodeURIComponent(runId)}/stream`);
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
      const ev = coerceEvent(msg.lastEventId || "", parsed);
      if (ev) setEvents((prev) => [...prev, ev]);
    };

    return () => {
      es.close();
      esRef.current = null;
    };
  }, [runId]);

  return { events, status };
}
