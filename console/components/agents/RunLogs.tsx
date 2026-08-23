"use client";

// components/agents/RunLogs.tsx — tabbed Run logs for the agent-detail drill-in (story 8.11).
//
// Five tabs projected from `run_events` (§7.1/§5.2): task/work-item (coordination record, Epic 2),
// tool-call, LLM (prompt/response + token counts), build output, and error traces. The BUILD tab
// deep-links to the build browser (8.7); each Run links to its OTel trace (§17.2/ISI-2133). An
// ACTIVE Run also gets a LIVE SSE log tail via the ONE shared stream (useRunStream, same BFF proxy
// as 8.2). READ-ONLY (R6): no mutate/claim/kill verb rides here — Kill Run is a separate control.

import { useEffect, useState } from "react";
import Link from "next/link";
import type { RunLogTab, RunPhase } from "@/lib/agents/types";
import { createAgentsClient, AgentsApiError } from "@/lib/agents/api";
import { useRunStream } from "@/lib/useRunStream";

const TABS: { id: RunLogTab; label: string }[] = [
  { id: "task", label: "Task / work-item" },
  { id: "tool", label: "Tool calls" },
  { id: "llm", label: "LLM" },
  { id: "build", label: "Build output" },
  { id: "error", label: "Errors" },
];

const ACTIVE_PHASES: ReadonlySet<RunPhase> = new Set([
  "Pending",
  "Claiming",
  "Running",
  "Paused",
]);

type TabState =
  | { kind: "loading" }
  | { kind: "not-found" }
  | { kind: "error" }
  | { kind: "ok"; entries: unknown[] };

function entryText(e: unknown): string {
  if (typeof e === "string") return e;
  if (e && typeof e === "object") {
    const r = e as Record<string, unknown>;
    if (typeof r.summary === "string") return r.summary;
    return JSON.stringify(r);
  }
  return String(e);
}

/** The live SSE tail for an active Run — the ONE shared EventSource against the BFF (8.2). */
function LiveTail({ runId }: { runId: string }) {
  const { events, status } = useRunStream(runId);
  return (
    <div className="run-logs__tail" aria-label="Live log tail">
      <header className="run-logs__tail-head">
        <span className={`stream-status stream-status--${status}`}>
          {status === "open" ? "live" : status}
        </span>
        <span className="muted">live tail (coordination record)</span>
      </header>
      {events.length === 0 ? (
        <p className="muted">Waiting for live events…</p>
      ) : (
        <ol className="run-logs__tail-list">
          {events.map((e, i) => (
            <li key={`${e.id}-${i}`} className="run-logs__tail-item">
              <span className="run-logs__tail-actor">{e.actor}</span>
              {e.summary && <span>{e.summary}</span>}
              <time className="run-logs__tail-ts">{e.ts}</time>
            </li>
          ))}
        </ol>
      )}
    </div>
  );
}

export function RunLogs({
  runId,
  phase,
  traceId,
}: {
  runId: string;
  phase?: RunPhase | null;
  traceId?: string | null;
}) {
  const [tab, setTab] = useState<RunLogTab>("task");
  const [state, setState] = useState<TabState>({ kind: "loading" });
  const isActive = phase != null && ACTIVE_PHASES.has(phase);

  useEffect(() => {
    let cancelled = false;
    setState({ kind: "loading" });
    const client = createAgentsClient();
    client
      .getRunLogs(runId, tab)
      .then((entries) => {
        if (!cancelled) setState({ kind: "ok", entries });
      })
      .catch((err: unknown) => {
        if (cancelled) return;
        if (err instanceof AgentsApiError && err.outcome === "not-found") {
          setState({ kind: "not-found" });
        } else {
          setState({ kind: "error" });
        }
      });
    return () => {
      cancelled = true;
    };
  }, [runId, tab]);

  return (
    <section className="run-logs" aria-label={`Logs for run ${runId}`}>
      <header className="run-logs__head">
        <div className="run-logs__tabs" role="tablist" aria-label="Run log tabs">
          {TABS.map((t) => (
            <button
              key={t.id}
              type="button"
              role="tab"
              aria-selected={tab === t.id}
              className={`run-logs__tab${tab === t.id ? " run-logs__tab--active" : ""}`}
              onClick={() => setTab(t.id)}
            >
              {t.label}
            </button>
          ))}
        </div>
        {traceId && (
          // Each Run links to its per-Run OTel trace (§17.2 / ISI-2133).
          <Link
            className="run-logs__trace"
            href={`/traces/${encodeURIComponent(traceId)}`}
          >
            View OTel trace ↗
          </Link>
        )}
      </header>

      {tab === "build" && (
        // The build tab is a legibility index; the bytes flow through the 8.7 build browser.
        <p className="run-logs__build-link">
          <Link href={`/runs/${encodeURIComponent(runId)}`}>
            Open the build browser for this run →
          </Link>
        </p>
      )}

      <div role="tabpanel" className="run-logs__panel">
        {state.kind === "loading" && <p className="muted">Loading {tab} logs…</p>}
        {state.kind === "not-found" && (
          <p className="muted">These logs are not available.</p>
        )}
        {state.kind === "error" && (
          <p className="muted">Couldn’t load {tab} logs. Try again.</p>
        )}
        {state.kind === "ok" &&
          (state.entries.length === 0 ? (
            <p className="muted">No {tab} entries.</p>
          ) : (
            <ol className="run-logs__entries">
              {state.entries.map((e, i) => (
                <li key={i} className="run-logs__entry">
                  <code>{entryText(e)}</code>
                </li>
              ))}
            </ol>
          ))}
      </div>

      {/* An active Run gets a live SSE log tail (8.2 one-bus discipline). */}
      {isActive && <LiveTail runId={runId} />}
    </section>
  );
}
