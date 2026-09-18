"use client";

// components/RunDetail.tsx — the run detail screen (ISI-4576): phase header, steps +
// thinking/comments timeline (Paperclip-style, per the approved mock run-detail-mock.html),
// artifacts panel and the live SSE stream.
//
// Data: GET /api/runs/{runId} detail read model (ISI-4571) via the BFF — run CRD + coord
// steps + progressmirror thinking/comments + LLM digests. Live updates ride the ONE shared
// EventSource (RunStream / useRunStream). Kill Run stays a separate control-plane POST
// (KillRun), never a stream verb (AC6).

import { useEffect, useState } from "react";

import { ArtifactBrowser } from "@/components/ArtifactBrowser";
import { KillRun } from "@/components/KillRun";
import { RunStream } from "@/components/RunStream";
import { phaseTone } from "@/components/SquadOverview";
import {
  fetchRunDetail,
  type RunDetailResponseWire,
  type RunDetailState,
  type RunStepWire,
  type ThinkingEntryWire,
} from "@/lib/runs";

type TimelineEntry =
  | { kind: "step"; ts: number; step: RunStepWire }
  | { kind: "thinking"; ts: number; entry: ThinkingEntryWire };

function parseTs(value?: string | null): number {
  if (!value) return 0;
  const t = Date.parse(value);
  return Number.isNaN(t) ? 0 : t;
}

function mergeTimeline(detail: RunDetailResponseWire): TimelineEntry[] {
  const entries: TimelineEntry[] = [];
  for (const step of detail.steps ?? []) {
    entries.push({ kind: "step", ts: parseTs(step.startedAt), step });
  }
  for (const entry of detail.thinking ?? []) {
    if (!entry.content) continue;
    entries.push({ kind: "thinking", ts: parseTs(entry.timestamp), entry });
  }
  entries.sort((a, b) => a.ts - b.ts);
  return entries;
}

function formatClock(ts: number): string {
  if (!ts) return "—";
  return new Date(ts).toLocaleString();
}

function formatDuration(startedAt?: string | null, terminal?: boolean): string | null {
  const start = parseTs(startedAt);
  if (!start) return null;
  const end = terminal ? start : Date.now();
  const seconds = Math.max(0, Math.round((end - start) / 1000));
  if (seconds < 60) return `${seconds}s`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m ${seconds % 60}s`;
  return `${Math.floor(minutes / 60)}h ${minutes % 60}m`;
}

function formatTokens(total?: number): string | null {
  if (!total || total <= 0) return null;
  if (total >= 1000) return `${(total / 1000).toFixed(1)}k tokens`;
  return `${total} tokens`;
}

const TERMINAL_PHASES = ["succeeded", "failed", "cancelled"];

function StepItem({ step }: { step: RunStepWire }) {
  const failed = step.status === "run_terminal" || Boolean(step.error);
  return (
    <div className="timeline-item">
      <div className={`timeline-marker ${failed ? "status-error" : "status-step"}`} />
      <div className="timeline-content-wrapper">
        <div className="timeline-header">
          <div className="timeline-title">{step.name || "step"}</div>
          <div className="timeline-time">{formatClock(parseTs(step.startedAt))}</div>
        </div>
        {step.description ? (
          <div className="timeline-description">from: {step.description}</div>
        ) : null}
        <div className="timeline-description">status: {step.status}</div>
        {step.error ? <div className="step-output">{step.error}</div> : null}
      </div>
    </div>
  );
}

function ThinkingItem({ entry }: { entry: ThinkingEntryWire }) {
  const isComment = entry.type === "comment";
  return (
    <div className="timeline-item">
      <div className={`timeline-marker ${isComment ? "status-comment" : "status-thinking"}`} />
      <div className="timeline-content-wrapper">
        <div className="timeline-header">
          <div className="timeline-title">
            {isComment ? "Agent Comment" : "Agent Thinking"}
          </div>
          <div className="timeline-time">{formatClock(parseTs(entry.timestamp))}</div>
        </div>
        {isComment ? (
          <div className="comment-entry">
            {entry.agent ? <div className="comment-author">{entry.agent}:</div> : null}
            <div className="comment-text">{entry.content}</div>
          </div>
        ) : (
          <div className="thinking-entry">
            {entry.agent ? <div className="comment-author">{entry.agent}</div> : null}
            <div className="thinking-text">{entry.content}</div>
          </div>
        )}
      </div>
    </div>
  );
}

export function RunDetail({ runId }: { runId: string }) {
  const [state, setState] = useState<RunDetailState>({ kind: "loading" });

  useEffect(() => {
    const ac = new AbortController();
    setState({ kind: "loading" });
    fetchRunDetail(runId, ac.signal).then((s) => {
      if (!ac.signal.aborted && s.kind !== "loading") setState(s);
    });
    return () => ac.abort();
  }, [runId]);

  if (state.kind === "loading") {
    return (
      <div className="card">
        <p className="muted">Loading run {runId}…</p>
      </div>
    );
  }
  if (state.kind === "error") {
    return (
      <div className="card">
        <h1 style={{ margin: 0 }}>Run {runId}</h1>
        <p className="muted">
          {state.status === 404
            ? "Run not found (or outside your team scope)."
            : state.status === 401
              ? "Sign in to view this run."
              : `Failed to load the run detail (HTTP ${state.status || "network error"}).`}
        </p>
      </div>
    );
  }

  const { run } = state.detail;
  const phase = run.status?.phase ?? "Unknown";
  const terminal = TERMINAL_PHASES.includes(phase.toLowerCase());
  const agents = (run.spec.agents ?? []).map((a) => a.name).filter(Boolean);
  const project = run.spec.projectRef?.name;
  const tokens = formatTokens(run.status?.totalTokenUsage?.totalTokens);
  const trace = run.status?.traceID;
  const startedAt = run.status?.claimedAt ?? run.metadata?.creationTimestamp ?? null;
  const duration = formatDuration(startedAt, terminal);
  const pausedReason = run.status?.conditions?.find((c) => c.type === "Paused")?.reason;
  const timeline = mergeTimeline(state.detail);

  return (
    <div className="run-detail">
      <header className="card run-detail__header">
        <div className="run-detail__header-row">
          <div className="run-detail__badges">
            <span className="phase-chip" data-tone={phaseTone(phase)} data-testid="run-phase">
              {phase}
              {pausedReason ? ` (${pausedReason})` : ""}
            </span>
            {agents.map((a) => (
              <span key={a} className="run-detail__agent-badge">
                {a}
              </span>
            ))}
            {project ? <span className="run-detail__project-badge">{project}</span> : null}
          </div>
          <KillRun workItem={run.spec.workItemRef ?? ""} phase={phase} />
        </div>
        <h1 style={{ margin: "8px 0 0" }}>Run {runId}</h1>
        <p className="muted" style={{ margin: "6px 0 0" }}>
          {startedAt ? `Started ${formatClock(parseTs(startedAt))}` : "Not started"}
          {duration ? ` · ${duration}` : ""}
          {tokens ? (
            <>
              {" · "}
              <span className="run-detail__token-count">{tokens}</span>
            </>
          ) : null}
          {trace ? ` · Trace: ${trace.slice(0, 12)}` : ""}
          {" · "}
          <a href={`/runs/${encodeURIComponent(runId)}/build`}>Build workspace</a>
        </p>
      </header>

      <div className="run-detail__grid">
        <div className="card timeline-content">
          <h2 style={{ margin: "0 0 8px" }}>Steps &amp; activity</h2>
          {timeline.length === 0 ? (
            <p className="muted">
              No steps or agent activity recorded yet — live events appear below as the run
              advances.
            </p>
          ) : (
            <div className="timeline" data-testid="run-timeline">
              {timeline.map((item, i) =>
                item.kind === "step" ? (
                  <StepItem key={`s-${item.step.id}-${i}`} step={item.step} />
                ) : (
                  <ThinkingItem key={`t-${item.entry.id}-${i}`} entry={item.entry} />
                ),
              )}
            </div>
          )}
        </div>

        <div className="run-detail__artifacts">
          <ArtifactBrowser runId={runId} />
        </div>
      </div>

      <div className="card">
        <RunStream runId={runId} />
      </div>
    </div>
  );
}
