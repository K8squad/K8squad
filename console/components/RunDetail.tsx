"use client";

// components/RunDetail.tsx — the run detail screen. Redesigned for ISI-4798 to the
// APPROVED ISI-4792 mock (docs/bmad/ux/isi-4792-agent-run-screen): a run-lifecycle
// rail up top, a 5-kind colour-coded activity stream (You / thinking / comment /
// tool / system), a This-run/All scope toggle, and a right rail of Artifacts +
// Run summary. Pure read-model reskin — no backend/schema change; every hue is an
// existing globals.css token (run-detail.css). Data still rides the shared
// EventSource (RunStream) and Kill Run stays a control-plane POST (KillRun).

import { useEffect, useMemo, useState } from "react";

import { ArtifactBrowser } from "@/components/ArtifactBrowser";
import { KillRun } from "@/components/KillRun";
import { RunStream } from "@/components/RunStream";
import { phaseTone } from "@/components/SquadOverview";
import "@/components/runs/run-detail.css";
import {
  buildLifecycle,
  classifyEntry,
  filterByScope,
  foldActivity,
  formatCompactTime,
  formatDurationMs,
  formatRailClock,
  runScopeLabel,
  type ActivityItem,
  type ClassifiedEntry,
  type EntryKind,
  type Lifecycle,
  type RunScope,
  type ToolCall,
} from "@/lib/run-activity";
import {
  fetchRunDetail,
  type RunDetailResponseWire,
  type RunDetailState,
} from "@/lib/runs";

const KIND_LABEL: Record<EntryKind, string> = {
  you: "You",
  thinking: "Agent thinking",
  comment: "Agent comment",
  tool: "Tool activity",
  system: "System",
};

const LEGEND: { kind: EntryKind; label: string }[] = [
  { kind: "thinking", label: "Thinking" },
  { kind: "comment", label: "Comment" },
  { kind: "you", label: "You" },
  { kind: "tool", label: "Tool" },
  { kind: "system", label: "System" },
];

function KindIcon({ kind }: { kind: EntryKind }) {
  // 16px stroke glyphs, one per kind — colour comes from the parent's currentColor.
  const common = {
    width: 16,
    height: 16,
    viewBox: "0 0 24 24",
    fill: "none",
    stroke: "currentColor",
    strokeWidth: 1.6,
    strokeLinecap: "round" as const,
    strokeLinejoin: "round" as const,
    "aria-hidden": true,
  };
  switch (kind) {
    case "you":
      return (
        <svg {...common}>
          <circle cx="12" cy="8" r="3.2" />
          <path d="M5.5 19a6.5 6.5 0 0 1 13 0" />
        </svg>
      );
    case "comment":
      return (
        <svg {...common}>
          <path d="M4 5h16v11H8l-4 3z" />
        </svg>
      );
    case "thinking":
      return (
        <svg {...common}>
          <path d="M4 6h16v9H9l-3 3v-3H4z" />
          <path d="M8 10h8M8 12.5h5" />
        </svg>
      );
    case "system":
      return (
        <svg {...common}>
          <circle cx="12" cy="12" r="3" />
          <path d="M12 3v3M12 18v3M3 12h3M18 12h3M5.6 5.6l2 2M16.4 16.4l2 2M18.4 5.6l-2 2M7.6 16.4l-2 2" />
        </svg>
      );
    case "tool":
    default:
      return (
        <svg {...common}>
          <path d="M14.5 6a3.5 3.5 0 0 0-4.9 4.4l-5 5 2 2 5-5A3.5 3.5 0 0 0 18 8.5L15.5 11 13 8.5 15.5 6z" />
        </svg>
      );
  }
}

function LifecycleRail({ lifecycle }: { lifecycle: Lifecycle }) {
  const { nodes, currentIndex, totalMs } = lifecycle;
  const total = formatDurationMs(totalMs);
  const fillPct = nodes.length > 1 ? (currentIndex / (nodes.length - 1)) * 100 : 0;
  return (
    <section className="card run-rail" data-testid="run-lifecycle">
      <div className="run-rail__head">
        <span className="run-rail__title">Run lifecycle</span>
        {total ? <span className="run-rail__total">total {total}</span> : null}
      </div>
      <div className="run-rail__track">
        <div className="run-rail__line" />
        <div className="run-rail__line run-rail__line--done" style={{ width: `${fillPct}%` }} />
        <ol className="run-rail__nodes">
          {nodes.map((n, i) => (
            <li
              key={n.label}
              className={`run-rail__node${n.reached ? " is-reached" : ""}${
                i === currentIndex ? " is-current" : ""
              }`}
            >
              <span className="run-rail__dot" />
              <span className="run-rail__label">{n.label}</span>
              <span className="run-rail__clock">{formatRailClock(n.clock)}</span>
            </li>
          ))}
        </ol>
      </div>
    </section>
  );
}

function ToolChip({ tool }: { tool: ToolCall }) {
  return (
    <span className={`tool-chip ${tool.ok ? "is-ok" : "is-err"}`}>
      <span className="tool-chip__mark" aria-hidden>
        {tool.ok ? "✓" : "✕"}
      </span>
      <span className="tool-chip__name">{tool.name}</span>
    </span>
  );
}

function ToolRow({ tools, ts }: { tools: ToolCall[]; ts: number }) {
  const errors = tools.filter((t) => !t.ok).length;
  return (
    <article className="activity" data-kind="tool" data-testid="activity-tool">
      <span className="activity__rail-dot" aria-hidden />
      <div className="activity__card">
        <header className="activity__head">
          <span className="activity__icon">
            <KindIcon kind="tool" />
          </span>
          <span className="activity__kind">Tool activity</span>
          <span className="activity__author">
            {tools.length} call{tools.length === 1 ? "" : "s"}
            {errors ? ` · ${errors} error${errors === 1 ? "" : "s"}` : ""}
          </span>
          <time className="activity__time">{formatCompactTime(ts)}</time>
        </header>
        <div className="activity__tools">
          {tools.map((t, i) => (
            <ToolChip key={`${t.name}-${i}`} tool={t} />
          ))}
        </div>
      </div>
    </article>
  );
}

function ActivityRow({ entry }: { entry: ClassifiedEntry }) {
  return (
    <article className="activity" data-kind={entry.kind} data-testid={`activity-${entry.kind}`}>
      <span className="activity__rail-dot" aria-hidden />
      <div className="activity__card">
        <header className="activity__head">
          <span className="activity__icon">
            <KindIcon kind={entry.kind} />
          </span>
          <span className="activity__kind">{KIND_LABEL[entry.kind]}</span>
          {entry.author ? <span className="activity__author">{entry.author}</span> : null}
          <time className="activity__time">{formatCompactTime(entry.ts)}</time>
        </header>
        <p className="activity__text">{entry.text}</p>
      </div>
    </article>
  );
}

const COLLAPSE_AFTER = 8;

function ActivityStream({
  items,
  emptyHint,
}: {
  items: ActivityItem[];
  emptyHint: string;
}) {
  const [expanded, setExpanded] = useState(false);
  if (items.length === 0) {
    return <p className="muted">{emptyHint}</p>;
  }
  const collapsed = !expanded && items.length > COLLAPSE_AFTER;
  const shown = collapsed ? items.slice(0, COLLAPSE_AFTER) : items;
  const hidden = items.length - shown.length;
  return (
    <div className="activity-stream" data-testid="run-timeline">
      {shown.map((item) =>
        item.type === "tools" ? (
          <ToolRow key={item.id} tools={item.tools} ts={item.ts} />
        ) : (
          <ActivityRow key={item.entry.id} entry={item.entry} />
        ),
      )}
      {collapsed ? (
        <button type="button" className="activity-more" onClick={() => setExpanded(true)}>
          {hidden} more activity item{hidden === 1 ? "" : "s"} collapsed · Show all
        </button>
      ) : null}
    </div>
  );
}

function RunSummaryCard({
  phase,
  phaseToneName,
  duration,
  tokens,
  steps,
  toolCount,
  toolErrors,
  agent,
}: {
  phase: string;
  phaseToneName: string;
  duration: string | null;
  tokens: string | null;
  steps: number;
  toolCount: number;
  toolErrors: number;
  agent: string | null;
}) {
  return (
    <section className="card run-summary" data-testid="run-summary">
      <h2 className="run-summary__title">Run summary</h2>
      <dl className="run-summary__rows">
        <div className="run-summary__row">
          <dt>Phase</dt>
          <dd data-tone={phaseToneName}>{phase}</dd>
        </div>
        <div className="run-summary__row">
          <dt>Duration</dt>
          <dd>{duration ?? "—"}</dd>
        </div>
        <div className="run-summary__row">
          <dt>Tokens</dt>
          <dd className="run-summary__tokens">{tokens ?? "—"}</dd>
        </div>
        <div className="run-summary__row">
          <dt>Steps</dt>
          <dd>{steps}</dd>
        </div>
        <div className="run-summary__row">
          <dt>Tools</dt>
          <dd>
            {toolCount}
            {toolErrors ? ` (${toolErrors} error${toolErrors === 1 ? "" : "s"})` : ""}
          </dd>
        </div>
        <div className="run-summary__row">
          <dt>Agent</dt>
          <dd className="run-summary__agent">{agent ?? "—"}</dd>
        </div>
      </dl>
    </section>
  );
}

function formatTokens(total?: number): string | null {
  if (!total || total <= 0) return null;
  if (total >= 1000) return `${(total / 1000).toFixed(1)}k`;
  return `${total}`;
}

function parseTs(value?: string | null): number {
  if (!value) return 0;
  const t = Date.parse(value);
  return Number.isNaN(t) ? 0 : t;
}

export function RunDetail({ runId }: { runId: string }) {
  const [state, setState] = useState<RunDetailState>({ kind: "loading" });
  const [scope, setScope] = useState<RunScope>("this");

  useEffect(() => {
    const ac = new AbortController();
    setState({ kind: "loading" });
    fetchRunDetail(runId, ac.signal).then((s) => {
      if (!ac.signal.aborted && s.kind !== "loading") setState(s);
    });
    return () => ac.abort();
  }, [runId]);

  const detail: RunDetailResponseWire | null = state.kind === "ready" ? state.detail : null;

  const classified = useMemo<ClassifiedEntry[]>(() => {
    if (!detail) return [];
    return (detail.thinking ?? [])
      .filter((e) => e.content)
      .map(classifyEntry)
      .sort((a, b) => a.ts - b.ts);
  }, [detail]);

  const scoped = useMemo(
    () => filterByScope(classified, scope, runId),
    [classified, scope, runId],
  );
  const items = useMemo(() => foldActivity(scoped), [scoped]);

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
  const agents = (run.spec.agents ?? []).map((a) => a.name).filter(Boolean) as string[];
  const project = run.spec.projectRef?.name;
  const tokens = formatTokens(run.status?.totalTokenUsage?.totalTokens);
  const trace = run.status?.traceID;
  const startedAt = run.status?.claimedAt ?? run.metadata?.creationTimestamp ?? null;
  const lifecycle = buildLifecycle(state.detail);
  const duration = formatDurationMs(lifecycle.totalMs);
  const pausedReason = run.status?.conditions?.find((c) => c.type === "Paused")?.reason;

  const toolEntries = classified.filter((e) => e.kind === "tool");
  const toolErrors = toolEntries.filter((e) => e.tool && !e.tool.ok).length;
  const label = runScopeLabel(runId);

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
        <h1 className="run-detail__title">Run {runId}</h1>
        <p className="run-detail__meta">
          {startedAt ? `Started ${new Date(parseTs(startedAt)).toLocaleString()}` : "Not started"}
          {duration ? ` · ${duration}` : ""}
          {tokens ? (
            <>
              {" · "}
              <span className="run-detail__token-count">{tokens} tokens</span>
            </>
          ) : null}
          {trace ? ` · Trace ${trace.slice(0, 8)}` : ""}
          {" · "}
          <a href={`/runs/${encodeURIComponent(runId)}/build`}>Build workspace</a>
        </p>
      </header>

      <div className="run-detail__grid">
        <div className="run-detail__main">
          <LifecycleRail lifecycle={lifecycle} />

          <section className="card run-activity">
            <div className="run-activity__head">
              <h2 className="run-activity__title">Activity</h2>
              <div
                className="run-scope"
                role="tablist"
                aria-label="Run scope"
                data-testid="run-scope-toggle"
              >
                <button
                  type="button"
                  role="tab"
                  aria-selected={scope === "this"}
                  className={`run-scope__btn${scope === "this" ? " is-active" : ""}`}
                  onClick={() => setScope("this")}
                  data-testid="run-scope-this"
                >
                  This run ({label})
                </button>
                <button
                  type="button"
                  role="tab"
                  aria-selected={scope === "all"}
                  className={`run-scope__btn${scope === "all" ? " is-active" : ""}`}
                  onClick={() => setScope("all")}
                  data-testid="run-scope-all"
                >
                  All
                </button>
              </div>
              <ul className="run-legend" aria-hidden>
                {LEGEND.map((l) => (
                  <li key={l.kind} className="run-legend__item" data-kind={l.kind}>
                    <span className="run-legend__dot" />
                    {l.label}
                  </li>
                ))}
              </ul>
            </div>
            <ActivityStream
              items={items}
              emptyHint={
                scope === "this" && classified.length > 0
                  ? "No activity for this run yet — switch to All to see other runs."
                  : "No agent activity recorded yet — live events appear below as the run advances."
              }
            />
          </section>
        </div>

        <aside className="run-detail__side">
          <ArtifactBrowser runId={runId} />
          <RunSummaryCard
            phase={phase}
            phaseToneName={phaseTone(phase)}
            duration={duration}
            tokens={tokens}
            steps={(state.detail.steps ?? []).length}
            toolCount={toolEntries.length}
            toolErrors={toolErrors}
            agent={agents[0] ?? null}
          />
        </aside>
      </div>

      <div className="card">
        <RunStream runId={runId} />
      </div>
    </div>
  );
}
