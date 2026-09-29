// lib/tickets/working.ts — ISI-5196 (S5 of ISI-5185, Track A): the ticket-view
// port of the discussion room's "an agent is working…" affordance (ISI-5174).
//
// ISI-5173 (Henrik): a human messages an agent on a TICKET and the Activity feed
// gives no conversational cue that the agent is actually working — the discussion
// room got that cue in ISI-5174 (the `{agent} is working…` → replied | could-not-
// respond pill), and this story brings the SAME affordance to the ticket view.
//
// REUSE, not re-derive: this module reuses the discussion reducer's public
// lifecycle vocabulary (`WorkingPhase`: working → replied | failed) and the ticket
// renders it with the SAME `ksq-working*` markup + a11y the discussion pill uses.
// What changes on the ticket is the SIGNAL SOURCE and one label enrichment:
//   - Source: the ticket already opens the per-run SSE bus (useDispatchWatch →
//     useRunStream); its honest ladder (queued/picking_up/working/succeeded/failed,
//     ISI-4853) IS the authoritative working state. We project that ladder onto the
//     discussion `WorkingPhase` rather than re-running the discussion room's blind
//     optimistic-seed + reply-correlation + timeout dance — that would be strictly
//     worse than the real run signal the ticket already holds, and it would open no
//     second stream (ISI-5174 AC4 "one bus, no polling"). Correlation limits from
//     ISI-5174 carry over: the minted run id never reaches the console as a message
//     stamp, so the pill tracks the ONE in-flight dispatch the ticket is watching.
//   - Label: the `working` label is EXTENDED to the current step verb the live
//     `thinking` rows report (e.g. "… is editing MessageItem.tsx…") as a PURE label
//     projection — the phase stays `working`; only the words get more specific.
//
// Pure — no fetch, no DOM (unit-tested in test/tickets/working.test.ts).

import type { WorkingPhase } from "@/lib/discussion/working";
import type { DispatchState } from "@/lib/tickets/useDispatchWatch";
import { parseProgressEnvelope } from "@/lib/tickets/progressEnvelope";

export type { WorkingPhase };

/** A minimal live `thinking` row (the subset of a run event this projection reads). */
export interface WorkingThinkingRow {
  /** the ProgressMirror envelope body (e.g. `[run …][tool:edit/start] MessageItem.tsx`). */
  summary?: string;
}

/** The rendered pill's data — the same triple the discussion WorkingIndicator paints. */
export interface TicketWorkingView {
  agentName: string;
  phase: WorkingPhase;
  /** the full, ready-to-render label (already de-noised; carries the step verb while working). */
  label: string;
}

/**
 * Project the ticket's honest run ladder (ISI-4853 DispatchState) onto the
 * discussion working vocabulary (ISI-5174 WorkingPhase):
 *   queued / picking_up / working → `working`   (the run is live / spinning up)
 *   succeeded                     → `replied`    (the run finished; its reply lands
 *                                                 as its own Activity comment)
 *   failed                        → `failed`     ("could not respond", AC3)
 * A ladder state we don't understand degrades to `working` (honest: something IS in
 * flight — we just can't say it finished).
 */
export function phaseFromDispatchState(state: DispatchState): WorkingPhase {
  switch (state) {
    case "succeeded":
      return "replied";
    case "failed":
      return "failed";
    default:
      return "working";
  }
}

// Present-progressive verbs for the crisp, structured tool signal. Mirrors the
// PAST-tense chip verbs in progressEnvelope.ts (TOOL_VERBS) but in the "…is Xing…"
// grammar the live pill needs. Keyed lower-case; the operator emits low-cardinality
// head tokens. An unknown tool falls back to a humanized "using <name>".
const TOOL_GERUNDS: Record<string, string> = {
  read: "reading",
  write: "writing",
  edit: "editing",
  multiedit: "editing",
  notebookedit: "editing a notebook",
  bash: "running a command",
  shell: "running a command",
  grep: "searching the code",
  glob: "matching files",
  ls: "listing files",
  find: "searching files",
  fetch: "fetching",
  webfetch: "fetching a page",
  websearch: "searching the web",
  task: "running a sub-agent",
  agent: "running a sub-agent",
  todowrite: "updating the plan",
  skill: "loading a skill",
};

/** The gerund clause for a tool name — "editing", "running a command", … */
function toolGerund(name: string): string {
  const key = name.trim().toLowerCase();
  const g = TOOL_GERUNDS[key];
  if (g) return g;
  if (!key) return "working";
  return "using " + key.replace(/[_/-]+/g, " ").trim();
}

/** Keep a tool target (usually a path/filename) short enough for a one-line pill. */
const STEP_TARGET_MAX = 48;

function shortTarget(summary: string): string {
  const firstLine = summary.split("\n")[0].trim();
  if (firstLine.length <= STEP_TARGET_MAX) return firstLine;
  return firstLine.slice(0, STEP_TARGET_MAX - 1).trimEnd() + "…";
}

/**
 * Derive the CURRENT step verb from the live `thinking` rows — the latest row is
 * the current step. Only a TOOL segment yields a verb (the crisp, structured
 * signal: "editing MessageItem.tsx"); narration / status / an unparseable row
 * carries no verb (→ the caller falls back to the plain "is working…" label), so
 * the pill never puts the agent's free-text prose into an "{agent} is …" frame
 * where it would read as broken grammar. Returns the verb clause or null.
 */
export function stepVerbFromThinking(
  rows: readonly WorkingThinkingRow[] | undefined,
): string | null {
  if (!rows || rows.length === 0) return null;
  const last = rows[rows.length - 1];
  const body = last?.summary ?? "";
  if (!body) return null;
  const seg = parseProgressEnvelope(body);
  if (seg.kind !== "tool") return null;
  const gerund = toolGerund(seg.name);
  const target = seg.summary ? shortTarget(seg.summary) : "";
  return target ? `${gerund} ${target}` : gerund;
}

/**
 * The pill label for a phase. `working` is the PURE label projection: the step
 * verb when the live thinking reports one, else the plain "is working…". `replied`
 * / `failed` mirror the discussion room's exact copy so the two surfaces read
 * identically.
 */
export function ticketWorkingLabel(
  agentName: string,
  phase: WorkingPhase,
  stepVerb: string | null,
): string {
  switch (phase) {
    case "replied":
      return `${agentName} replied`;
    case "failed":
      return `${agentName} could not respond`;
    case "working":
      return stepVerb
        ? `${agentName} is ${stepVerb}…`
        : `${agentName} is working…`;
  }
}

/**
 * Compose the ticket working pill from the dispatched agent name, the ticket's
 * honest ladder state, and the live thinking rows. Returns null when there is no
 * agent to attribute the work to (nothing to render). The step verb only enriches
 * the `working` phase; `replied`/`failed` ignore it (the phase already tells the
 * whole truth).
 */
export function ticketWorkingView(
  agentName: string,
  state: DispatchState,
  thinking: readonly WorkingThinkingRow[] | undefined,
): TicketWorkingView | null {
  if (!agentName) return null;
  const phase = phaseFromDispatchState(state);
  const stepVerb = phase === "working" ? stepVerbFromThinking(thinking) : null;
  return {
    agentName,
    phase,
    label: ticketWorkingLabel(agentName, phase, stepVerb),
  };
}
