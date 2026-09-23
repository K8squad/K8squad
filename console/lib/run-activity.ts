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

import type {
  LLMInteractionWire,
  RunDetailResponseWire,
  RunStepWire,
  ThinkingEntryWire,
} from "@/lib/runs";

// `llm` = a model prompt/response exchange; `tool` = a tool_use call or its
// observation output. Both are EXECUTION kinds (ISI-4813 P2-C): the run screen
// separates these from the CONVERSATION kinds (you / comment / system) so
// ticket/admin chatter no longer drowns the execution detail.
export type EntryKind = "you" | "thinking" | "comment" | "tool" | "system" | "llm";

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
  /** For `kind === "tool"`: whether this entry is the call (args) or its result (output). */
  toolPhase?: "call" | "result";
  /** For `kind === "llm"`: the interaction role joined from the digest (prompt|response|…). */
  role?: string;
  /** For `kind === "llm"`: token count joined from the digest, when known. */
  tokens?: number;
  ts: number;
}

/** Execution = the model's own work; Conversation = ticket/admin chatter. */
export const EXECUTION_KINDS: ReadonlySet<EntryKind> = new Set(["llm", "tool", "thinking"]);
export const CONVERSATION_KINDS: ReadonlySet<EntryKind> = new Set(["you", "comment", "system"]);

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
 * Map a wire thinking/comment/tool entry to its display kind (§3 table).
 *
 * Precedence (ISI-4813): the enriched P2-A activity types come first — the
 * backend only ever tags `llm_interaction` / `tool_use` / `observation` on the
 * model-exchange entries it derives from `Run.Status.LLMInteractions`
 * (activityFromInteraction), and `comment` on progressmirror rows. A legacy
 * `[tool:…]` marker still wins for older payloads. Only then do the human/system
 * agent prefixes and the comment/thinking fallback apply.
 */
export function classifyEntry(entry: ThinkingEntryWire): ClassifiedEntry {
  const raw = entry.content ?? "";
  const { runScope, rest } = parseRunScope(raw);
  const agent = (entry.agent ?? "").trim();
  const ts = parseTs(entry.timestamp);
  const base = { id: entry.id, runScope, ts };

  // 1. A legacy `[tool:name/result(..)]` marker wins over everything — it can ride
  //    any agent (a pasted result), matching the pre-P2-C behaviour.
  const marker = parseToolCall(rest);
  if (marker) {
    return {
      ...base,
      kind: "tool",
      toolPhase: "call",
      tool: marker.tool,
      text: marker.rest,
      author: agent || undefined,
    };
  }
  // 2. Human / system turns are keyed off the agent, ahead of the execution types —
  //    a real prompt/tool/observation entry always carries the model as its agent
  //    (activityFromInteraction), never `user:*` or the operator, so this only
  //    guards against a mis-typed conversation row.
  if (isUserAgent(agent)) {
    return { ...base, kind: "you", author: userName(agent), text: rest };
  }
  if (isSystemAgent(agent)) {
    return { ...base, kind: "system", author: agent || undefined, text: rest };
  }
  // 3. The enriched P2-A execution types.
  if (entry.type === "tool_use") {
    return {
      ...base,
      kind: "tool",
      toolPhase: "call",
      tool: { name: "tool", ok: true },
      text: rest,
      author: agent || undefined,
    };
  }
  if (entry.type === "observation") {
    return { ...base, kind: "tool", toolPhase: "result", text: rest, author: agent || undefined };
  }
  if (entry.type === "llm_interaction") {
    return { ...base, kind: "llm", text: rest, author: agent || undefined };
  }
  if (entry.type === "thinking") {
    return { ...base, kind: "thinking", text: rest, author: agent || undefined };
  }
  // 4. Conversation fallback.
  if (entry.type === "comment") {
    return { ...base, kind: "comment", author: agent || undefined, text: rest };
  }
  return { ...base, kind: "thinking", author: agent || undefined, text: rest };
}

/**
 * Join the `llmInteractions` digest onto the classified `llm` entries by id, so
 * the model-exchange card can show the role and token count the thinking entry
 * itself does not carry (ISI-4813). Non-llm entries pass through untouched; a
 * missing digest leaves role/tokens undefined (honest — no fabrication).
 */
export function enrichLlm(
  entries: ClassifiedEntry[],
  digests: LLMInteractionWire[] | null | undefined,
): ClassifiedEntry[] {
  if (!digests?.length) return entries;
  const byId = new Map(digests.map((d) => [d.id, d]));
  return entries.map((e) => {
    if (e.kind !== "llm") return e;
    const d = byId.get(e.id);
    if (!d) return e;
    return {
      ...e,
      role: d.role || e.role,
      tokens: d.tokensUsed && d.tokensUsed > 0 ? d.tokensUsed : e.tokens,
      author: d.model || e.author,
    };
  });
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

// ---- execution vs conversation (ISI-4813 P2-C) -----------------------------

export type RunView = "execution" | "conversation";

/** Split the classified feed into the execution and conversation streams. */
export function partitionByView(entries: ClassifiedEntry[]): {
  execution: ClassifiedEntry[];
  conversation: ClassifiedEntry[];
} {
  const execution: ClassifiedEntry[] = [];
  const conversation: ClassifiedEntry[] = [];
  for (const e of entries) {
    if (CONVERSATION_KINDS.has(e.kind)) conversation.push(e);
    else execution.push(e);
  }
  return { execution, conversation };
}

/** One rendered row in the execution stream (ISI-4814 §5 component decomposition). */
export type ExecutionItem =
  | {
      type: "exchange";
      id: string;
      model?: string;
      tokens?: number;
      prompt?: string;
      response?: string;
      ts: number;
    }
  | {
      type: "tool";
      id: string;
      name?: string;
      ok?: boolean;
      args?: string;
      output?: string;
      ts: number;
    }
  | { type: "thinking"; id: string; entry: ClassifiedEntry };

/**
 * Project the execution entries into cards. Adjacent halves are paired so each
 * card reads as one unit: an `llm` prompt followed by its response becomes one
 * model-exchange card (blue prompt + green response panels); a tool `call`
 * followed by its `result` becomes one tool card (args + output + exit badge).
 * Unpaired halves render on their own — the screen never fabricates a missing
 * side.
 */
export function buildExecutionItems(entries: ClassifiedEntry[]): ExecutionItem[] {
  const out: ExecutionItem[] = [];
  for (let i = 0; i < entries.length; i++) {
    const e = entries[i];
    const next = entries[i + 1];

    if (e.kind === "llm") {
      const item: Extract<ExecutionItem, { type: "exchange" }> = {
        type: "exchange",
        id: e.id,
        model: e.author,
        tokens: e.tokens,
        ts: e.ts,
      };
      const isResponse = (e.role ?? "").toLowerCase() === "response";
      if (isResponse) item.response = e.text;
      else item.prompt = e.text;
      // Fold a following response into the same card when this was the prompt.
      if (!isResponse && next?.kind === "llm" && (next.role ?? "").toLowerCase() === "response") {
        item.response = next.text;
        item.tokens = next.tokens ?? item.tokens;
        item.model = next.author ?? item.model;
        i++;
      }
      out.push(item);
      continue;
    }

    if (e.kind === "tool") {
      const item: Extract<ExecutionItem, { type: "tool" }> = {
        type: "tool",
        id: e.id,
        name: e.tool?.name,
        ok: e.tool?.ok,
        ts: e.ts,
      };
      if (e.toolPhase === "result") {
        item.output = e.text;
      } else {
        item.args = e.text;
        // Fold the paired observation output into the same card.
        if (next?.kind === "tool" && next.toolPhase === "result") {
          item.output = next.text;
          item.ts = next.ts;
          i++;
        }
      }
      out.push(item);
      continue;
    }

    out.push({ type: "thinking", id: e.id, entry: e });
  }
  return out;
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
