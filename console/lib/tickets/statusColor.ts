// lib/tickets/statusColor.ts — the single source of truth for phase-status colour
// (ISI-4452 plan §5 S1, design ISI-4452 §1 · ISI-4431 phase-lifecycle model).
//
// ISI-4431 replaces the current 5-lane board with a 10-status, phase-based
// lifecycle. Each status maps to a project phase group (Intake · Build · Review ·
// Done), an owning Role, and a hue. Both the List (ISI-4456) and the Kanban
// re-skin (ISI-4457, gated on the ISI-4455 enum) tint from THIS table so the two
// views can never drift apart.
//
// Hues are pinned byte-for-byte to design ISI-4452 §1 (which mirrors the ISI-4455
// backend enum table). Where a phase reuses a locked story-8.9 token
// (globals.css) the CSS custom property aliases it; the four mid-phase extension
// hues (planning / code_review / testing / documentation) are net-new shared
// tokens with the exact hex from the table. `blocked` is a CONDITION, not a
// status (§8.6) — its rose overlay hue lives here for reuse but never appears as
// a phase.
//
// This helper is deliberately tolerant of the CURRENT 5-value read model
// (`backlog` · `todo` · `in_progress` · `in_review` · `done`, lib/tickets/types
// `WORK_ITEM_STATES`): those legacy states resolve to the closest phase hue so
// the redesign renders correctly TODAY, before ISI-4455 lands the 10-status enum.
// An unknown state falls back to a neutral idle chip — never a fabricated colour.

/** The phase groups the 10 statuses cluster under (design §1, Kanban super-headers). */
export type PhaseGroup = "Intake" | "Build" | "Review" | "Done";

export interface StatusMeta {
  /** Human label for the chip / legend. */
  label: string;
  /** CSS custom property reference for the hue (e.g. "var(--ksq-phase-backlog)"). */
  hue: string;
  /** The phase group this status clusters under. */
  group: PhaseGroup;
  /** Owning Role, or null for the un-owned intake/terminal phases. */
  role: string | null;
  /** `cancelled` renders muted + struck-through (design §1 row 10). */
  struck?: boolean;
}

/**
 * The 10 canonical phase statuses in lifecycle order (design ISI-4452 §1). The
 * List legend (S5) walks this array; the Kanban re-skin (ISI-4457) will use it
 * for its column order once ISI-4455 lands the matching backend enum.
 */
export const PHASE_STATUSES = [
  "backlog",
  "todo",
  "design",
  "planning",
  "implementation",
  "code_review",
  "testing",
  "documentation",
  "done",
  "cancelled",
] as const;

export type PhaseStatus = (typeof PHASE_STATUSES)[number];

/**
 * The six lifecycle WORKING phases — the middle of the 10-status enum, in order.
 * These are the ONLY statuses a Role can be bound to via `activePhases` (ISI-4431
 * E3 §1) and the only statuses that count as "a phase" for the honest phase
 * affordance (FR-7 / ISI-4487): `backlog`/`todo` are intake lanes and
 * `done`/`cancelled` are terminal lanes — none is ever *worked* by a role, so a
 * ticket sitting on one has NO phase (the UI says "— no phase" rather than fabricate
 * one). The `satisfies` guard pins every value to a real PhaseStatus so this set can
 * never drift from the enum without a compile error.
 */
export const WORKING_PHASES = [
  "design",
  "planning",
  "implementation",
  "code_review",
  "testing",
  "documentation",
] as const satisfies readonly PhaseStatus[];

export type WorkingPhase = (typeof WORKING_PHASES)[number];

/**
 * Status → presentation metadata. Keyed by the 10 phase statuses PLUS the two
 * legacy read-model states (`in_progress` / `in_review`) that still ride the
 * current enum until ISI-4455 lands — each mapped to the closest phase hue so
 * live data renders in colour today.
 */
export const STATUS_META: Record<string, StatusMeta> = {
  backlog: { label: "Backlog", hue: "var(--ksq-phase-backlog)", group: "Intake", role: null },
  todo: { label: "Todo", hue: "var(--ksq-phase-todo)", group: "Intake", role: null },
  design: { label: "Design", hue: "var(--ksq-phase-design)", group: "Build", role: "Graphic Designer" },
  planning: { label: "Planning", hue: "var(--ksq-phase-planning)", group: "Build", role: "Architect" },
  implementation: {
    label: "Implementation",
    hue: "var(--ksq-phase-implementation)",
    group: "Build",
    role: "Front-End Engineer",
  },
  code_review: {
    label: "Code Review",
    hue: "var(--ksq-phase-code_review)",
    group: "Review",
    role: "Code Reviewer",
  },
  testing: { label: "Testing", hue: "var(--ksq-phase-testing)", group: "Review", role: "Testing Architect" },
  documentation: {
    label: "Documentation",
    hue: "var(--ksq-phase-documentation)",
    group: "Review",
    role: "Tech Writer",
  },
  done: { label: "Done", hue: "var(--ksq-phase-done)", group: "Done", role: null },
  cancelled: { label: "Cancelled", hue: "var(--ksq-phase-cancelled)", group: "Done", role: null, struck: true },

  // Legacy 5-lane states (current read model, pre-ISI-4455) → closest phase hue.
  in_progress: {
    label: "In Progress",
    hue: "var(--ksq-phase-implementation)",
    group: "Build",
    role: null,
  },
  in_review: { label: "In Review", hue: "var(--ksq-phase-code_review)", group: "Review", role: null },
};

/** Neutral fallback for a status the read model returns that we don't recognise. */
const FALLBACK: StatusMeta = {
  label: "Unknown",
  hue: "var(--ksq-phase-backlog)",
  group: "Intake",
  role: null,
};

/** Full presentation metadata for a status (label · hue · group · role). */
export function statusMeta(state: string): StatusMeta {
  return STATUS_META[state] ?? { ...FALLBACK, label: humanize(state) };
}

/** Just the hue token for a status — the common case (spine / dot / chip tint). */
export function statusColor(state: string): string {
  return statusMeta(state).hue;
}

/** Title-case an unknown snake_case status so the fallback chip still reads. */
function humanize(state: string): string {
  return state
    .split("_")
    .map((w) => (w ? w[0].toUpperCase() + w.slice(1) : w))
    .join(" ") || "—";
}
