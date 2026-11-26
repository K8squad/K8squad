"use client";

// components/tickets/FeedTreatments.tsx - ISI-7888 (design ISI-7861): the
// conversation-first feed's four non-comment treatments. TicketDetail keeps
// owning the data (the NormalizedThread read + the live SSE rows it already
// holds); these components only PRESENT:
//   - FeedModeToggle - the [ Conversation | All activity ] segmented control
//     (Conversation default, per-user persistence, para-5);
//   - ActivityGroupChip - >=2 lane/state events collapsed to one faint chip,
//     expandable in place (para-5);
//   - SystemNoticeCard - the run-failure one-liner (warning Run failed -
//     model route) with expandable evidence + View run (para-5; never a
//     comment card);
//   - RunStreamPanel - the collapsed bar on the owning agent comment ("Live:
//     agent working... step N", pulse dot) expanding to a newest-first step
//     list, hydrated from the ISI-7484 durable store (GET /api/runs/{id}/
//     events) merged with the live rows the ticket already streams - live
//     AND post-run replay, with NO second EventSource.
// Expand/collapse choices persist per user, per affordance (para-5); new
// events on a collapsed group update the count without auto-expanding.
// Known v1 gap (honest, FR-I3-style): the notice card's "Retry run" action
// is not wired - a confirm-gated re-dispatch primitive does not exist on
// this surface yet; the notice's own Next steps line names the real action.

import { useCallback, useEffect, useMemo, useState } from "react";
import { runHref } from "@/lib/discussion/provenance";
import {
  envelopeSnippet,
  parseProgressEnvelope,
} from "@/lib/tickets/progressEnvelope";
import {
  noticeHeadline,
  noticeRunId,
  readStoredExpanded,
  readStoredFeedMode,
  storeExpanded,
  storeFeedMode,
  type FeedMode,
} from "@/lib/tickets/feedView";
import type { ActivityItem, LiveThinkingRow } from "@/lib/tickets/thread";
import { displayName } from "@/lib/tickets/runComments";
import { Markdown } from "@/components/markdown/Markdown";
import { STATE_LABELS, type WorkItemState } from "@/lib/tickets/types";

function stateLabel(state: string): string {
  return STATE_LABELS[state as WorkItemState] ?? state;
}

function fmt(ts: string): string {
  const t = Date.parse(ts);
  return Number.isNaN(t) ? "just now" : new Date(t).toLocaleString();
}

/** The [ Conversation | All activity ] segmented control (para-5).
 * Conversation is the default; the choice persists per user. */
export function FeedModeToggle({
  mode,
  onChange,
}: {
  mode: FeedMode;
  onChange: (mode: FeedMode) => void;
}) {
  return (
    <div
      className="ksq-feed-toggle"
      role="tablist"
      aria-label="Feed view"
      data-testid="feed-mode-toggle"
    >
      <button
        type="button"
        role="tab"
        aria-selected={mode === "conversation"}
        className={`ksq-feed-toggle__opt${
          mode === "conversation" ? " ksq-feed-toggle__opt--active" : ""
        }`}
        data-testid="feed-mode-conversation"
        onClick={() => onChange("conversation")}
      >
        Conversation
      </button>
      <button
        type="button"
        role="tab"
        aria-selected={mode === "all"}
        className={`ksq-feed-toggle__opt${
          mode === "all" ? " ksq-feed-toggle__opt--active" : ""
        }`}
        data-testid="feed-mode-all"
        onClick={() => onChange("all")}
      >
        All activity
      </button>
    </div>
  );
}

/** The per-user feed tab state: conversation default, stored choice applied
 * after mount (SSR-safe - the stored value is read client-side only, the
 * ProjectsNavTree rail pattern). */
export function useFeedMode(): [FeedMode, (mode: FeedMode) => void] {
  const [mode, setMode] = useState<FeedMode>("conversation");
  useEffect(() => {
    setMode(readStoredFeedMode());
  }, []);
  const change = useCallback((next: FeedMode) => {
    setMode(next);
    storeFeedMode(next);
  }, []);
  return [mode, change];
}

/** One lane/state event rendered as the thin timeline line the ActivityRow
 * event branch owns - shared by the group's expanded list. */
function EventLine({ item }: { item: ActivityItem }) {
  const e = item.event!;
  return (
    <>
      <span className="ksq-activity__dot" aria-hidden="true" />
      <span className="muted">
        {e.principal ? displayName(e.principal) : "someone"} moved{" "}
        {stateLabel(e.fromState)} → {stateLabel(e.toState)}
      </span>
      <time className="ksq-ticket-id">{fmt(item.at)}</time>
    </>
  );
}

/** The activity group (para-4 #5, faintest weight): >=2 consecutive
 * lane/state events collapsed to ONE chip; expanding reveals the timeline IN
 * PLACE (no navigation). New events update the count, never auto-expand. */
export function ActivityGroupChip({
  items,
  scopeId,
}: {
  items: ActivityItem[];
  scopeId: string;
}) {
  const [open, setOpen] = useState(false);
  useEffect(() => {
    setOpen(readStoredExpanded(scopeId));
  }, [scopeId]);
  const n = items.length;
  return (
    <li className="ksq-activity-group" data-testid="feed-activity-group">
      <button
        type="button"
        className="ksq-activity-group__toggle"
        data-testid="feed-activity-toggle"
        aria-expanded={open}
        onClick={() => {
          const next = !open;
          setOpen(next);
          storeExpanded(scopeId, next);
        }}
      >
        <span className="ksq-activity-group__caret" aria-hidden="true">
          {open ? "▾" : "▸"}
        </span>
        <span className="ksq-activity-group__dot" aria-hidden="true" />
        <span className="ksq-activity-group__label">Activity</span>
        <span className="muted ksq-activity-group__count">
          {n} {n === 1 ? "event" : "events"}
        </span>
      </button>
      {open && (
        <ol
          className="ksq-activity-group__events"
          data-testid="feed-activity-body"
        >
          {items.map((it, i) => (
            <li
              key={i}
              className="ksq-activity ksq-activity--event ksq-activity-group__event"
              data-testid="activity-event"
            >
              <EventLine item={it} />
            </li>
          ))}
        </ol>
      )}
    </li>
  );
}

/** The system-notice card (para-4 #3): a compact muted one-liner with an
 * amber tick - warning Run failed - model route + Show details - expanding
 * IN PLACE to the full evidence (the notice body verbatim, rich-rendered:
 * run_id, terminal step, cause, a2a settle, patch SHAs, artifacts, next
 * steps) and the View run deep-link when the evidence names a run. Never
 * styled like a comment card, never an avatar. */
export function SystemNoticeCard({
  item,
  scopeId,
}: {
  item: ActivityItem;
  scopeId: string;
}) {
  const body = item.comment?.body ?? "";
  const headline = noticeHeadline(body);
  const runId = noticeRunId(body);
  const [open, setOpen] = useState(false);
  useEffect(() => {
    setOpen(readStoredExpanded(scopeId));
  }, [scopeId]);
  return (
    <li className="ksq-notice" data-testid="feed-notice">
      <button
        type="button"
        className="ksq-notice__toggle"
        data-testid="feed-notice-toggle"
        aria-expanded={open}
        onClick={() => {
          const next = !open;
          setOpen(next);
          storeExpanded(scopeId, next);
        }}
      >
        <span className="ksq-notice__tick" aria-hidden="true">
          ⚠
        </span>
        <span className="ksq-notice__headline">{headline}</span>
        <span className="muted ksq-notice__details">
          {open ? "▾ details" : "Show details ▾"}
        </span>
        <time className="ksq-ticket-id ksq-notice__time">{fmt(item.at)}</time>
      </button>
      {open && (
        <div className="ksq-notice__body" data-testid="feed-notice-body">
          <Markdown
            className="ksq-notice__markdown"
            testId="feed-notice-markdown"
          >
            {body}
          </Markdown>
          {runId && (
            <a
              className="ksq-notice__runlink"
              href={runHref(runId)}
              data-testid="feed-notice-runlink"
              title={`Full run ID: ${runId}`}
            >
              View run
            </a>
          )}
        </div>
      )}
    </li>
  );
}

/** One streamed agent step (durable or live): mono time + snippet. */
type StreamStep = { at: string; body: string };

const epochDesc = (s: string): number => {
  const t = Date.parse(s);
  return Number.isNaN(t) ? 0 : t;
};

/** The streaming panel (para-4 #4): a collapsed bar on the OWNING agent
 * comment - "Live: agent working... step N" with a pulse dot while the run
 * is in flight, "Agent work - N steps - run finished" after. Expanding
 * reveals the newest-first step list. Hydration is the ISI-7484 durable
 * store: one GET /api/runs/{id}/events on mount (replay - works after the
 * run ended, even on a cold reload) MERGED with the live rows the ticket
 * surface already streams (dispatchWatch + ambient tails) - no second
 * EventSource (ISI-5174 AC4). Deduped by (at, body) like
 * mergeLiveThinking. */
export function RunStreamPanel({
  runId,
  running,
  liveSteps,
  agentName,
  scopeId,
}: {
  runId: string;
  running: boolean;
  liveSteps: LiveThinkingRow[];
  agentName: string;
  scopeId: string;
}) {
  const [open, setOpen] = useState(false);
  const [replay, setReplay] = useState<StreamStep[]>([]);
  useEffect(() => {
    setOpen(readStoredExpanded(scopeId));
  }, [scopeId]);
  useEffect(() => {
    let alive = true;
    fetch(`/api/runs/${encodeURIComponent(runId)}/events`, {
      headers: { accept: "application/json" },
      cache: "no-store",
    })
      .then((res) => (res.ok ? res.json() : null))
      .then((payload: unknown) => {
        if (!alive || payload === null) return;
        const rows = (payload as { events?: unknown }).events;
        if (!Array.isArray(rows)) return;
        const steps: StreamStep[] = [];
        for (const row of rows) {
          if (typeof row !== "object" || row === null) continue;
          const r = row as { type?: unknown; at?: unknown; data?: unknown };
          if (r.type !== "thinking") continue;
          const d = (r.data ?? {}) as { at?: unknown; body?: unknown };
          if (typeof d.body !== "string" || d.body === "") continue;
          const at =
            typeof d.at === "string" && d.at
              ? d.at
              : typeof r.at === "string"
                ? r.at
                : "";
          steps.push({ at, body: d.body });
        }
        setReplay(steps);
      })
      .catch(() => {
        // best-effort replay - the live rows still cover the run's session
      });
    return () => {
      alive = false;
    };
  }, [runId]);

  const steps = useMemo(() => {
    const seen = new Set<string>();
    const out: StreamStep[] = [];
    for (const s of [...replay, ...liveSteps]) {
      if (!s.body) continue;
      const k = `${s.at}::${s.body}`;
      if (seen.has(k)) continue;
      seen.add(k);
      out.push({ at: s.at, body: s.body });
    }
    return out.sort((a, b) => epochDesc(b.at) - epochDesc(a.at));
  }, [replay, liveSteps]);

  const count = steps.length;
  return (
    <div
      className="ksq-runstream"
      data-testid="run-stream-panel"
      data-running={running ? "true" : "false"}
    >
      <button
        type="button"
        className="ksq-runstream__toggle"
        data-testid="run-stream-toggle"
        aria-expanded={open}
        onClick={() => {
          const next = !open;
          setOpen(next);
          storeExpanded(scopeId, next);
        }}
      >
        <span
          className={`ksq-runstream__dot${
            running ? " ksq-runstream__dot--live" : ""
          }`}
          aria-hidden="true"
        />
        <span className="ksq-runstream__label">
          {running
            ? `Live: ${agentName} working… step ${count}`
            : `Agent work · ${count} ${
                count === 1 ? "step" : "steps"
              } · run finished`}
        </span>
        <span className="ksq-runstream__caret" aria-hidden="true">
          {open ? "▾" : "▸"}
        </span>
      </button>
      {open && (
        <ol className="ksq-runstream__steps" data-testid="run-stream-steps">
          {steps.map((s, i) => (
            <li key={i} className="ksq-runstream__step">
              <time className="ksq-ticket-id ksq-runstream__step-time">
                {fmt(s.at)}
              </time>
              <span className="ksq-runstream__step-body">
                {envelopeSnippet(parseProgressEnvelope(s.body))}
              </span>
            </li>
          ))}
          {count === 0 && (
            <li className="muted ksq-runstream__step">No steps recorded.</li>
          )}
        </ol>
      )}
    </div>
  );
}
