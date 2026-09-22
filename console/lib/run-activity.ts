// lib/run-activity.ts — the read-model classifier + projections behind the ISI-4792
// run-detail redesign (built for ISI-4798). PURE functions over the existing
// `GET /api/runs/{runId}` detail read model (lib/runs.ts) — zero backend/schema
// change. RunDetail.tsx renders what these return.
//
// The design (DESIGN-SPEC-ISI-4792 §3-4) asks for:
//   - a 5-kind colour system (You / thinking / comment / tool / system) derived
//     purely by classifying each wire entry;
//   - tool calls parsed out of `[tool:<name>/result(ok|err)]` payloads and folded
//     into one compact chip row instead of a card per call;
//   - a `This run` / `All` scope filter keyed off the `[run <id>]` prefix each
//     entry already carries, so interleaved runs stop mixing;
//   - a lifecycle rail projected out of the run phase + step transitions.

import type { RunDetailResponseWire, RunStepWire, ThinkingEntryWire } from "@/lib/runs";

export type EntryKind = "you" | "thinking" | "comment" | "tool" | "system";

export interface ToolCall {
  name: string;
  ok: boolean;
}

export interface ClassifiedEntry {
  id: string;
  kind: EntryKind;
  /** Content with the `[run …]`/`[tool:…]` prefixes stripped. */
  text: string;
  author?: string;
  /** The run token parsed from a `[run <id>]` prefix, if any. */
  runScope: string | null;
  /** Present only for `kind === "tool"`. */
  tool?: ToolCall;
  ts: number;
}

function parseTs(value?: string | null): number {
  if (!value) return 0;
  const t = Date.parse(value);
  return Number.isNaN(t) ? 0 : t;
}

/** Strip a leading `[run <id>]` marker, returning the token and the remainder. */
export function parseRunScope(content: string): { runScope: string | null; rest: string } {
  const m = /^\s*\[run\s+([A-Za-z0-9._-]+)\]\s*/.exec(content ?? "");
  if (!m) return { runScope: null, rest: (content ?? "").trim() };
  return { runScope: m[1], rest: content.slice(m[0].length).trim() };
}

const OK_RESULTS = new Set(["ok", "success", "succeeded", "pass"]);

/** Parse a `[tool:<name>/result(ok|err)]` marker off the front of the content. */
export function parseToolCall(content: string): { tool: ToolCall; rest: string } | null {
  const m = /^\s*\[tool:([A-Za-z0-9._-]+)\/result\(([A-Za-z]+)\)\]\s*/.exec(content ?? "");
  if (!m) return null;
  const ok = OK_RESULTS.has(m[2].toLowerCase());
  return { tool: { name: m[1], ok }, rest: content.slice(m[0].length).trim() };
}

function isUserAgent(agent: string): boolean {
  const a = agent.toLowerCase();
  return /^user[:/]/.test(a) || a === "you" || a === "human" || a === "user" || a === "admin";
}

function isSystemAgent(agent: string): boolean {
  const a = agent.toLowerCase();
  return (
    a === "ksquad-operator" ||
    a === "system" ||
    a === "operator" ||
    /^run\//.test(a) ||
    /operator$/.test(a)
  );
}

/** The display name for a human turn (`user:admin` → `admin`). */
function userName(agent: string): string | undefined {
  const m = /^user[:/](.+)$/i.exec(agent);
  if (m) return m[1];
  return agent || undefined;
}

/**
 * Map a wire thinking/comment/tool entry to its display kind + tone (§3 table).
 * Precedence: a tool marker wins (it can ride any agent), then the human/system
 * agent prefixes, then the `comment` vs `thinking` type.
 */
export function classifyEntry(entry: ThinkingEntryWire): ClassifiedEntry {
  const raw = entry.content ?? "";
  const { runScope, rest } = parseRunScope(raw);
  const agent = (entry.agent ?? "").trim();
  const ts = parseTs(entry.timestamp);
  const base = { id: entry.id, runScope, ts };

  const tool = parseToolCall(rest);
  if (tool || entry.type === "tool_use") {
    const name = tool?.tool.name ?? "tool";
    const ok = tool?.tool.ok ?? true;
    return { ...base, kind: "tool", tool: { name, ok }, text: tool?.rest ?? rest, author: agent || undefined };
  }
  if (isUserAgent(agent)) {
    return { ...base, kind: "you", author: userName(agent), text: rest };
  }
  if (isSystemAgent(agent)) {
    return { ...base, kind: "system", author: agent || undefined, text: rest };
  }
  if (entry.type === "comment") {
    return { ...base, kind: "comment", author: agent || undefined, text: rest };
  }
  return { ...base, kind: "thinking", author: agent || undefined, text: rest };
}

/** The short run label shown on the `This run (r15)` toggle. */
export function runScopeLabel(runId: string): string {
  const m = /-(r\d+)$/i.exec(runId ?? "");
  if (m) return m[1];
  const short = (runId ?? "").split("-").pop() ?? runId ?? "";
  return short.length > 8 ? short.slice(0, 8) : short;
}

/**
 * Whether an entry belongs to the run currently being viewed. Unlabelled entries
 * (no `[run …]` prefix) are treated as this run's own; a labelled entry belongs
 * only when its token matches the run id or its short label — so r15's stream
 * stops interleaving with 51c621b4 / af093ad5.
 */
export function belongsToRun(entry: ClassifiedEntry, runId: string): boolean {
  const tok = entry.runScope;
  if (!tok) return true;
  const r = (runId ?? "").toLowerCase();
  const t = tok.toLowerCase();
  const label = runScopeLabel(runId).toLowerCase();
  return r.includes(t) || t === label || t.includes(label) || label.includes(t);
}

export type RunScope = "this" | "all";

export function filterByScope(
  entries: ClassifiedEntry[],
  scope: RunScope,
  runId: string,
): ClassifiedEntry[] {
  if (scope === "all") return entries;
  return entries.filter((e) => belongsToRun(e, runId));
}

/** A rendered activity row: either a single entry or a folded run of tool calls. */
export type ActivityItem =
  | { type: "entry"; entry: ClassifiedEntry }
  | { type: "tools"; id: string; tools: ToolCall[]; ts: number };

/** Fold consecutive `tool` entries into one compact chip row (§2 tool activity). */
export function foldActivity(entries: ClassifiedEntry[]): ActivityItem[] {
  const out: ActivityItem[] = [];
  let bucket: ClassifiedEntry[] = [];
  const flush = () => {
    if (!bucket.length) return;
    out.push({
      type: "tools",
      id: `tools-${bucket[0].id}`,
      tools: bucket.map((b) => b.tool!).filter(Boolean),
      ts: bucket[bucket.length - 1].ts,
    });
    bucket = [];
  };
  for (const e of entries) {
    if (e.kind === "tool") {
      bucket.push(e);
      continue;
    }
    flush();
    out.push({ type: "entry", entry: e });
  }
  flush();
  return out;
}

// ---- lifecycle rail --------------------------------------------------------

export interface LifecycleNode {
  label: string;
  clock: number | null;
  /** Reached = the run has progressed to or past this phase. */
  reached: boolean;
}

export interface Lifecycle {
  nodes: LifecycleNode[];
  /** Index (0-3) of the phase the run currently sits at. */
  currentIndex: number;
  totalMs: number | null;
}

const TERMINAL_PHASES = new Set(["succeeded", "failed", "cancelled", "completed"]);

function phaseIndex(phase: string): number {
  const p = (phase ?? "").toLowerCase();
  if (TERMINAL_PHASES.has(p)) return 3;
  if (p === "collecting" || p === "collect") return 2;
  if (p === "running" || p === "executing" || p === "claimed") return 1;
  return 0; // dispatching / pending / unknown
}

/**
 * Project the four-node lifecycle rail (Dispatching → Running → Collecting →
 * <terminal>) out of the run phase + step timestamps — replacing the wall of
 * state-transition rows the stream used to carry.
 */
export function buildLifecycle(detail: RunDetailResponseWire): Lifecycle {
  const run = detail.run;
  const phase = run.status?.phase ?? "";
  const currentIndex = phaseIndex(phase);
  const terminal = TERMINAL_PHASES.has(phase.toLowerCase());

  const steps: RunStepWire[] = detail.steps ?? [];
  const started = steps.map((s) => parseTs(s.startedAt)).filter(Boolean);
  const completed = steps.map((s) => parseTs(s.completedAt)).filter(Boolean);
  const created = parseTs(run.metadata?.creationTimestamp);
  const claimed = parseTs(run.status?.claimedAt);

  const dispatch = created || claimed || (started.length ? Math.min(...started) : 0);
  const running = claimed || (started.length ? Math.min(...started) : 0);
  const collecting = completed.length ? Math.max(...completed) : 0;
  const terminalClock = terminal
    ? completed.length
      ? Math.max(...completed)
      : running || dispatch
    : 0;

  const terminalLabel = terminal
    ? phase.charAt(0).toUpperCase() + phase.slice(1)
    : "Succeeded";

  const nodes: LifecycleNode[] = [
    { label: "Dispatching", clock: dispatch || null, reached: currentIndex >= 0 },
    { label: "Running", clock: running || null, reached: currentIndex >= 1 },
    { label: "Collecting", clock: collecting || null, reached: currentIndex >= 2 },
    { label: terminalLabel, clock: terminalClock || null, reached: currentIndex >= 3 },
  ];

  const end = terminalClock || collecting || Date.now();
  const totalMs = dispatch ? Math.max(0, end - dispatch) : null;

  return { nodes, currentIndex, totalMs };
}

// ---- formatting ------------------------------------------------------------

function pad2(n: number): string {
  return n < 10 ? `0${n}` : `${n}`;
}

/** Compact `h:mm` timestamp for activity rows (§2.4). */
export function formatCompactTime(ts: number): string {
  if (!ts) return "—";
  const d = new Date(ts);
  return `${d.getHours()}:${pad2(d.getMinutes())}`;
}

/** `h:mm:ss` clock for the lifecycle rail nodes. */
export function formatRailClock(ts: number | null): string {
  if (!ts) return "—";
  const d = new Date(ts);
  return `${d.getHours()}:${pad2(d.getMinutes())}:${pad2(d.getSeconds())}`;
}

export function formatDurationMs(ms: number | null): string | null {
  if (ms == null) return null;
  const seconds = Math.max(0, Math.round(ms / 1000));
  if (seconds < 60) return `${seconds}s`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m ${pad2(seconds % 60)}s`;
  return `${Math.floor(minutes / 60)}h ${pad2(minutes % 60)}m`;
}
