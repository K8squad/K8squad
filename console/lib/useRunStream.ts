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
  | "CHECKOUT"
  | "COMMENT"
  | "HANDOFF"
  | "MEMORY"
  | "ARTIFACT"
  | "STEP"
  | "LIFECYCLE"
  | "THINKING";

export type RunEvent = {
  id: string;
  kind: RunEventKind;
  actor: string; // agent·role, server-stamped
  ts: string; // server timestamp (client-stamped for outbox events, which carry no ts)
  summary?: string;
  /**
   * The ISI-7098 wire tier (WS-1) carried on a thinking frame payload
   * ('thinking' | 'deliverable'): thinking = ephemeral in-progress reasoning
   * (the italic live tier), deliverable = the run ONE final answer (the
   * clean-comment tier). Absent = a legacy envelope, treated as thinking for
   * display. Carried as 'tier' so it never collides with kind: RunEventKind.
   */
  tier?: "thinking" | "deliverable";
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
const LIFECYCLE_EVENTS = [
  "assigned",
  "scheduled",
  "sandbox_bound",
  "started",
  "ended",
] as const;
/**
 * `event: thinking` — the operator's progress mirror echoed as a run event
 * (ISI-5193, plan ISI-5185/WS-A). Payload is the SAME triple the durable
 * coord.comment row holds so the ticket can live-append a row identical to the
 * one a later thread reload materializes: {runId, author, body, seq, at}. Carried
 * on THIS per-run stream — no second EventSource (ISI-5174 AC4).
 */
const THINKING_EVENT = "thinking";
/**
 * `event: ticket_comment` — a DURABLE work-item comment echoed as a run event
 * (ISI-7274, parent ISI-7262 WS-2). Payload mirrors the durable coord.comment
 * triple {runId, workItemId, author, body, kind, at}: the agent's final
 * task-io comment (kind 'summary' / plain) captured post-commit, and the
 * settle's terminal rows co-committed with their INSERT. The ticket surface
 * re-fetches the thread when this frame arrives, so the final comment
 * materializes live without racing the run's `ended` milestone. Reuses the
 * legacy "COMMENT" kind: these rows are durable thread comments, not ephemeral.
 */
const COMMENT_EVENT = "ticket_comment";

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

/** Shape a `thinking` payload into a RunEvent, or null when it carries no body
 * (author or body missing ⇒ nothing to render). `at` is the mirror's UTC stamp;
 * fall back to now so the row still sorts sensibly if an older backend omits it. */
function thinkingEvent(id: string, raw: unknown): RunEvent | null {
  if (typeof raw !== "object" || raw === null) return null;
  const r = raw as Record<string, unknown>;
  const author = typeof r.author === "string" ? r.author : "";
  const body = typeof r.body === "string" ? r.body : "";
  if (!body) return null;
  // ISI-7098 tier on the envelope: 'deliverable' marks the run ONE final
  // answer; 'thinking' (and anything absent/unknown) is the in-progress tier.
  const tier =
    r.kind === "thinking" || r.kind === "deliverable" ? r.kind : undefined;
  return {
    id,
    kind: "THINKING",
    actor: author || "unknown",
    ts: typeof r.at === "string" && r.at ? r.at : new Date().toISOString(),
    summary: body,
    ...(tier ? { tier } : {}),
  };
}

/** Shape a `ticket_comment` payload into a RunEvent (kind COMMENT), or null
 * when it carries no body (nothing durable to materialize ⇒ ignore). `at` is
 * the comment's server-stamped created_at; fall back to now so the row still
 * sorts sensibly if an older backend omits it. */
function commentEvent(id: string, raw: unknown): RunEvent | null {
  if (typeof raw !== "object" || raw === null) return null;
  const r = raw as Record<string, unknown>;
  const body = typeof r.body === "string" ? r.body : "";
  if (!body) return null;
  return {
    id,
    kind: "COMMENT",
    actor: typeof r.author === "string" && r.author ? r.author : "unknown",
    ts: typeof r.at === "string" && r.at ? r.at : new Date().toISOString(),
    summary: body,
  };
}

/**
 * One row of the durable hydration tail (GET /api/runs/{id}/events, ISI-7484):
 * the same id/type/data triple the SSE projector frames carry, plus the row's
 * commit stamp so hydrated history keeps true timestamps.
 */
type RunHistoryRow = {
  id?: unknown;
  type?: unknown;
  data?: unknown;
  at?: unknown;
};

/**
 * Numeric event id (the outbox row id) for ordered inserts; NaN for legacy
 * frames that carry no id (those append in arrival order).
 */
function numericId(id: string): number {
  const n = Number(id);
  return Number.isFinite(n) ? n : Number.NaN;
}

/**
 * Insert ev into prev deduped by id and, when ids are numeric, in id order —
 * the overlap between hydration and live frames collapses exactly like an SSE
 * reconnect replay (same id ⇒ same event, drop it). This is the ISI-7484
 * hydration/live seam: the REST tail and the SSE tail share the outbox row id.
 */
function insertOrdered(prev: RunEvent[], ev: RunEvent): RunEvent[] {
  if (ev.id && prev.some((p) => p.id === ev.id)) return prev;
  if (!ev.id) return [...prev, ev];
  const n = numericId(ev.id);
  if (Number.isNaN(n)) return [...prev, ev];
  let i = prev.length;
  while (i > 0) {
    const p = numericId(prev[i - 1].id);
    if (Number.isNaN(p) || p <= n) break;
    i--;
  }
  return [...prev.slice(0, i), ev, ...prev.slice(i)];
}

/**
 * Shape a hydrated history row with the SAME mappers the live listeners use,
 * so a hydrated row renders identically to its live twin (and dedupes against
 * it by id). The hydrated lifecycle/step rows keep the row's commit stamp (at);
 * thinking/comment rows read their own payload stamp, exactly like live.
 * Unknown types are skipped, exactly like unlistened SSE event names.
 */
function shapeHistoryRow(row: unknown): RunEvent | null {
  if (typeof row !== "object" || row === null) return null;
  const r = row as RunHistoryRow;
  if (typeof r.id !== "string" || typeof r.type !== "string") return null;
  const at = typeof r.at === "string" && r.at ? r.at : new Date().toISOString();
  if (r.type === THINKING_EVENT) return thinkingEvent(r.id, r.data);
  if (r.type === COMMENT_EVENT) return commentEvent(r.id, r.data);
  if (r.type === RECONCILE_EVENT) {
    return {
      id: r.id,
      kind: "STEP",
      actor: "reconciler",
      ts: at,
      summary: stepSummary(r.data),
    };
  }
  if ((LIFECYCLE_EVENTS as readonly string[]).includes(r.type)) {
    const name = r.type;
    const step = stepSummary(r.data);
    return {
      id: r.id,
      kind: "LIFECYCLE",
      actor: lifecycleActor(r.data),
      ts: at,
      summary: step ? `${name}: ${step}` : name,
    };
  }
  return null;
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

    // ISI-7484 (WS-3, parents ISI-7482/ISI-7480): durable hydration. Fetch the
    // run's outbox tail once on mount and seed the SAME list the live frames
    // append to — a reload / late join never shows an empty Activity for a run
    // that already produced steps. Best-effort by design: a failed or non-OK
    // fetch leaves the live tail (and its Last-Event-ID reconnect replay) as
    // the fallback; overlap dedupes by the shared outbox row id.
    let hydrateCancelled = false;
    fetch(`/api/runs/${encodeURIComponent(runId)}/events`, {
      headers: { accept: "application/json" },
      cache: "no-store",
    })
      .then((res) => (res.ok ? res.json() : null))
      .then((body: unknown) => {
        if (hydrateCancelled || body === null) return;
        const rows = (body as { events?: unknown }).events;
        if (!Array.isArray(rows)) return;
        for (const row of rows) push(shapeHistoryRow(row));
      })
      .catch(() => {
        // best-effort hydration — the SSE tail still covers everything live
      });

    es.onopen = () => setStatus("open");
    es.onerror = () => setStatus("error"); // native EventSource auto-reconnects w/ Last-Event-ID

    const parse = (msg: MessageEvent): unknown | null => {
      try {
        return JSON.parse(msg.data);
      } catch {
        return null; // ignore non-JSON keepalives
      }
    };
    // ISI-7484: id-deduped, id-ordered insert — hydration rows and live frames
    // share the outbox row id, so a frame replayed after hydration (or arriving
    // while the hydration fetch is in flight) is dropped instead of duplicated,
    // and an older hydrated row lands BEFORE newer live rows even when the
    // fetch resolves late.
    const push = (ev: RunEvent | null) => {
      if (ev) setEvents((prev) => insertOrdered(prev, ev));
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

    // Live mirrored comment: `event: thinking` (ISI-5193). Same triple as the durable
    // coord.comment row so the ticket live-appends a row it can dedup on reload.
    const onThinking = (msg: MessageEvent) => {
      const parsed = parse(msg);
      if (parsed !== null) push(thinkingEvent(msg.lastEventId || "", parsed));
    };
    es.addEventListener(THINKING_EVENT, onThinking);

    // Live durable-comment echo: `event: ticket_comment` (ISI-7274, parent
    // ISI-7262 WS-2). The payload is the durable comment triple, not a
    // rendering row; consumers re-fetch the thread to materialize it.
    const onComment = (msg: MessageEvent) => {
      const parsed = parse(msg);
      if (parsed !== null) push(commentEvent(msg.lastEventId || "", parsed));
    };
    es.addEventListener(COMMENT_EVENT, onComment);

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
      es.removeEventListener(THINKING_EVENT, onThinking);
      es.removeEventListener(COMMENT_EVENT, onComment);
      for (const [name, listener] of lifecycleListeners) {
        es.removeEventListener(name, listener);
      }
      hydrateCancelled = true;
      es.close();
      esRef.current = null;
    };
  }, [runId]);

  return { events, status };
}
