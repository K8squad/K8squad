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
//
// Event vocabulary (ISI-4576): the backend emits NAMED SSE events off coord.outbox
// (internal/apiserver/runevents.go runEventToSSE — `event:` = the outbox event_type):
//   - `reconcile_advanced` — coarse step advance, payload {from_step, to_step, fence_token}
//   - lifecycle milestones (ADR-0021 D4, pkg/coord/prodreconcile.go): `assigned`,
//     `scheduled`, `sandbox_bound`, `started`, `ended` — payload
//     {schema, event, from_step, to_step, fence_token, run_id, agent, project, team, …}
// Named events never fire `onmessage`, so each type gets its own listener. The legacy
// kind-stamped payloads (CHECKOUT/COMMENT/HANDOFF/MEMORY/ARTIFACT on the default message
// channel) stay accepted for backwards compatibility.

import { useEffect, useRef, useState } from "react";

/** Server-stamped coordination-event kinds (AC7), plus the reconcile/lifecycle vocabulary. */
export type RunEventKind =
  | "CHECKOUT" | "COMMENT" | "HANDOFF" | "MEMORY" | "ARTIFACT"
  | "STEP" | "LIFECYCLE";

export type RunEvent = {
  id: string;
  kind: RunEventKind;
  actor: string; // agent·role, server-stamped
  ts: string; // server timestamp (client-stamped for outbox events, which carry no ts)
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

/** Named SSE event types emitted by the outbox projector (event: line = outbox event_type). */
const RECONCILE_EVENT = "reconcile_advanced";
const LIFECYCLE_EVENTS = ["assigned", "scheduled", "sandbox_bound", "started", "ended"] as const;

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

function stepSummary(raw: unknown): string | undefined {
  if (typeof raw !== "object" || raw === null) return undefined;
  const r = raw as Record<string, unknown>;
  const from = typeof r.from_step === "string" ? r.from_step : "";
  const to = typeof r.to_step === "string" ? r.to_step : "";
  if (!from && !to) return undefined;
  return from ? `${from} → ${to}` : to;
}

function lifecycleActor(raw: unknown): string {
  if (typeof raw !== "object" || raw === null) return "reconciler";
  const r = raw as Record<string, unknown>;
  return typeof r.agent === "string" && r.agent ? r.agent : "reconciler";
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

    const parse = (msg: MessageEvent): unknown | null => {
      try {
        return JSON.parse(msg.data);
      } catch {
        return null; // ignore non-JSON keepalives
      }
    };
    const push = (ev: RunEvent | null) => {
      if (ev) setEvents((prev) => [...prev, ev]);
    };

    // Legacy kind-stamped payloads on the default message channel.
    es.onmessage = (msg: MessageEvent) => {
      const parsed = parse(msg);
      if (parsed !== null) push(coerceEvent(msg.lastEventId || "", parsed));
    };

    // Coarse step advance: `event: reconcile_advanced`.
    const onAdvanced = (msg: MessageEvent) => {
      const parsed = parse(msg);
      if (parsed === null) return;
      push({
        id: msg.lastEventId || "",
        kind: "STEP",
        actor: "reconciler",
        ts: new Date().toISOString(),
        summary: stepSummary(parsed),
      });
    };
    es.addEventListener(RECONCILE_EVENT, onAdvanced);

    // Discrete lifecycle milestones (ADR-0021 D4): assigned/scheduled/sandbox_bound/started/ended.
    const lifecycleListeners = LIFECYCLE_EVENTS.map((name) => {
      const listener = (msg: MessageEvent) => {
        const parsed = parse(msg);
        if (parsed === null) return;
        const step = stepSummary(parsed);
        push({
          id: msg.lastEventId || "",
          kind: "LIFECYCLE",
          actor: lifecycleActor(parsed),
          ts: new Date().toISOString(),
          summary: step ? `${name}: ${step}` : name,
        });
      };
      es.addEventListener(name, listener);
      return [name, listener] as const;
    });

    return () => {
      es.removeEventListener(RECONCILE_EVENT, onAdvanced);
      for (const [name, listener] of lifecycleListeners) {
        es.removeEventListener(name, listener);
      }
      es.close();
      esRef.current = null;
    };
  }, [runId]);

  return { events, status };
}
