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
  buildExecutionItems,
  buildLifecycle,
  classifyEntry,
  enrichLlm,
  filterByScope,
  foldActivity,
  formatCompactTime,
  formatDurationMs,
  formatRailClock,
  partitionByView,
  runScopeLabel,
  type ActivityItem,
  type ClassifiedEntry,
  type EntryKind,
  type ExecutionItem,
  type Lifecycle,
  type RunScope,
  type RunView,
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
  llm: "Model exchange",
};

const EXECUTION_LEGEND: { kind: EntryKind; label: string }[] = [
  { kind: "llm", label: "Model" },
  { kind: "tool", label: "Tool" },
  { kind: "thinking", label: "Thinking" },
];

const CONVERSATION_LEGEND: { kind: EntryKind; label: string }[] = [
  { kind: "you", label: "You" },
  { kind: "comment", label: "Comment" },
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
    case "llm":
      return (
        <svg {...common}>
          <path d="M4 5h16v10H10l-4 3v-3H4z" />
          <path d="M8 9h8M8 11.5h5" />
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

// ---- execution view (ISI-4813 P2-C / ISI-4814 mock) ------------------------

const EXCHANGE_COLLAPSE = 480; // chars of a panel shown before "Show all"

function ExchangePanel({ label, tone, text }: { label: string; tone: "prompt" | "response"; text: string }) {
  const [open, setOpen] = useState(false);
  const long = text.length > EXCHANGE_COLLAPSE;
  const shown = open || !long ? text : `${text.slice(0, EXCHANGE_COLLAPSE)}…`;
  return (
    <div className={`exchange__panel exchange__panel--${tone}`}>
      <span className="exchange__panel-label">{label}</span>
      <p className="exchange__panel-text">{shown}</p>
      {long ? (
        <button type="button" className="exchange__toggle" onClick={() => setOpen((v) => !v)}>
          {open ? "Show less" : "Show all"}
        </button>
      ) : null}
    </div>
  );
}

function ModelExchangeCard({ item }: { item: Extract<ExecutionItem, { type: "exchange" }> }) {
  const tokens = formatTokens(item.tokens);
  return (
    <article className="activity" data-kind="llm" data-testid="activity-llm">
      <span className="activity__rail-dot" aria-hidden />
      <div className="activity__card exchange">
        <header className="activity__head">
          <span className="activity__icon">
            <KindIcon kind="llm" />
          </span>
          <span className="activity__kind">Model exchange</span>
          {item.model ? <span className="activity__author">{item.model}</span> : null}
          {tokens ? <span className="exchange__tokens">{tokens} tokens</span> : null}
          <time className="activity__time">{formatCompactTime(item.ts)}</time>
        </header>
        {item.prompt ? <ExchangePanel label="Prompt" tone="prompt" text={item.prompt} /> : null}
        {item.response ? <ExchangePanel label="Response" tone="response" text={item.response} /> : null}
        {!item.prompt && !item.response ? (
          <p className="activity__text muted">No exchange content recorded.</p>
        ) : null}
      </div>
    </article>
  );
}

function ToolCallCard({ item }: { item: Extract<ExecutionItem, { type: "tool" }> }) {
  const [open, setOpen] = useState(false);
  const hasExit = typeof item.ok === "boolean";
  return (
    <article className="activity" data-kind="tool" data-testid="activity-tool">
      <span className="activity__rail-dot" aria-hidden />
      <div className="activity__card toolcall">
        <button
          type="button"
          className="toolcall__head"
          aria-expanded={open}
          onClick={() => setOpen((v) => !v)}
        >
          <span className="activity__icon">
            <KindIcon kind="tool" />
          </span>
          <span className="activity__kind">Tool call</span>
          {item.name && item.name !== "tool" ? (
            <code className="toolcall__name">{item.name}</code>
          ) : null}
          {hasExit ? (
            <span className={`toolcall__exit ${item.ok ? "is-ok" : "is-err"}`}>
              {item.ok ? "ok" : "error"}
            </span>
          ) : null}
          <time className="activity__time">{formatCompactTime(item.ts)}</time>
          <span className="toolcall__caret" aria-hidden>
            {open ? "▾" : "▸"}
          </span>
        </button>
        {open ? (
          <div className="toolcall__body">
            {item.args ? (
              <div className="toolcall__section">
                <span className="toolcall__label">Args</span>
                <pre className="toolcall__pre">{item.args}</pre>
              </div>
            ) : null}
            {item.output ? (
              <div className="toolcall__section">
                <span className="toolcall__label">Output</span>
                <pre className="toolcall__pre">{item.output}</pre>
              </div>
            ) : (
              <p className="activity__text muted">No tool output recorded for this call.</p>
            )}
          </div>
        ) : null}
      </div>
    </article>
  );
}

function ExecutionStream({ items }: { items: ExecutionItem[] }) {
  const [expanded, setExpanded] = useState(false);
  if (items.length === 0) {
    return (
      <div className="execution-empty" data-testid="execution-empty">
        <p className="execution-empty__line">No model exchange recorded for this run.</p>
        <p className="execution-empty__line">No tool calls recorded for this run.</p>
        <p className="execution-empty__hint muted">
          Execution telemetry (prompts, tool calls) appears here once the run emits it.
        </p>
      </div>
    );
  }
  const collapsed = !expanded && items.length > COLLAPSE_AFTER;
  const shown = collapsed ? items.slice(0, COLLAPSE_AFTER) : items;
  const hidden = items.length - shown.length;
  return (
    <div className="activity-stream" data-testid="run-execution">
      {shown.map((item) =>
        item.type === "exchange" ? (
          <ModelExchangeCard key={item.id} item={item} />
        ) : item.type === "tool" ? (
          <ToolCallCard key={item.id} item={item} />
        ) : (
          <ActivityRow key={item.id} entry={item.entry} />
        ),
      )}
      {collapsed ? (
        <button type="button" className="activity-more" onClick={() => setExpanded(true)}>
          {hidden} more execution item{hidden === 1 ? "" : "s"} collapsed · Show all
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
  const [view, setView] = useState<RunView>("execution");

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
    const entries = (detail.thinking ?? [])
      .filter((e) => e.content)
      .map(classifyEntry)
      .sort((a, b) => a.ts - b.ts);
    return enrichLlm(entries, detail.llmInteractions);
  }, [detail]);

  const scoped = useMemo(
    () => filterByScope(classified, scope, runId),
    [classified, scope, runId],
  );
  const { execution, conversation } = useMemo(() => partitionByView(scoped), [scoped]);
  const executionItems = useMemo(() => buildExecutionItems(execution), [execution]);
  const conversationItems = useMemo(() => foldActivity(conversation), [conversation]);

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

  // Run-wide tool tally for the summary: fold call+result halves so one tool call
  // counts once (not twice), across the whole run regardless of the scope toggle.
  const allToolCards = buildExecutionItems(partitionByView(classified).execution).filter(
    (i) => i.type === "tool",
  );
  const toolCount = allToolCards.length;
  const toolErrors = allToolCards.filter((i) => i.type === "tool" && i.ok === false).length;
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
                className="run-view"
                role="tablist"
                aria-label="Run view"
                data-testid="run-view-toggle"
              >
                <button
                  type="button"
                  role="tab"
                  aria-selected={view === "execution"}
                  className={`run-view__btn${view === "execution" ? " is-active" : ""}`}
                  onClick={() => setView("execution")}
                  data-testid="run-view-execution"
                >
                  Run execution
                </button>
                <button
                  type="button"
                  role="tab"
                  aria-selected={view === "conversation"}
                  className={`run-view__btn${view === "conversation" ? " is-active" : ""}`}
                  onClick={() => setView("conversation")}
                  data-testid="run-view-conversation"
                >
                  Conversation
                  {conversation.length ? ` (${conversation.length})` : ""}
                </button>
              </div>
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
                {(view === "execution" ? EXECUTION_LEGEND : CONVERSATION_LEGEND).map((l) => (
                  <li key={l.kind} className="run-legend__item" data-kind={l.kind}>
                    <span className="run-legend__dot" />
                    {l.label}
                  </li>
                ))}
              </ul>
            </div>
            {view === "execution" ? (
              <ExecutionStream items={executionItems} />
            ) : (
              <ActivityStream
                items={conversationItems}
                emptyHint={
                  scope === "this" && conversation.length === 0 && classified.length > 0
                    ? "No conversation for this run yet — switch to All to see other runs."
                    : "No ticket conversation recorded for this run."
                }
              />
            )}
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
            toolCount={toolCount}
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
