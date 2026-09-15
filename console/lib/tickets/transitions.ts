// lib/tickets/transitions.ts — the CLIENT-side phase-transition guard (ISI-4457 S4).
//
// ISI-4455 landed the authoritative server graph: it is deliberately GENEROUS —
// every move is accepted except a direct hop between the two terminals
// (done ↔ cancelled → HTTP 422 ErrTransitionNotAllowed) so the deployed 5-lane
// board never breaks mid-migration. The Kanban owns a STRICTER, guard-aware set
// of drop affordances (ISI-4455 landing comment §2): a disallowed phase jump
// shows a "no-drop" cue and issues NO PATCH (fail-closed). Because the client is
// strictly stricter than the server, any drop we allow that the server rejects
// only ever returns 422 — surfaced as a re-sync notice, never a lie.
//
// The column model + hues are owned by lib/tickets/statusColor (S1, the shared
// SSOT). This module adds ONLY the adjacency graph + the legacy-lane fold — it
// never forks the colour map.

import { PHASE_STATUSES, WORKING_PHASES, type PhaseStatus, type WorkingPhase } from "./statusColor";

/**
 * Fold a read-model `state` to the phase COLUMN it renders under. The coordinator's
 * dispatch engine still writes the two transitional lanes (`in_progress` /
 * `in_review`) via claim/settle/reroute (retired later on the ISI-4431 track); they
 * are folded for display, never shown as their own column. A phase status maps to
 * itself. An unrecognised state has no column (dropped, never guessed — mirrors
 * derivation's unknown-state discipline).
 */
const LEGACY_FOLD: Readonly<Record<string, PhaseStatus>> = {
  in_progress: "implementation",
  in_review: "code_review",
};

export function phaseOf(state: string): PhaseStatus | null {
  if ((PHASE_STATUSES as readonly string[]).includes(state)) return state as PhaseStatus;
  return LEGACY_FOLD[state] ?? null;
}

/**
 * The WORKING phase a ticket sits on for the honest phase affordance (FR-7 /
 * ISI-4487), folding the two legacy engine lanes (in_progress→implementation,
 * in_review→code_review via phaseOf). Returns null for the intake lanes
 * (backlog/todo), the terminal lanes (done/cancelled), and any unknown state —
 * none of those is a *worked* phase, so the UI shows "— no phase" rather than
 * fabricate a "Design". A legacy ticket on a non-coordinator team never sits on a
 * phase lane, so it too resolves to null: the honesty is entirely state-driven, no
 * team/coordinator lookup required. This never GUESSES a phase — same discipline as
 * phaseOf dropping (rather than inventing a column for) an unknown state.
 */
export function workingPhaseOf(state: string): WorkingPhase | null {
  const p = phaseOf(state);
  return p && (WORKING_PHASES as readonly string[]).includes(p) ? (p as WorkingPhase) : null;
}

/**
 * Client-side allowed drop targets per phase (ISI-4455 landing comment §2 —
 * forward + rework adjacency). Terminal phases only reopen to the intake lanes.
 * `cancelled` is a valid target from every working lane; it is never reachable
 * directly from `done` (and vice-versa) — that is the one hop the server 422s.
 */
export const ALLOWED_DROPS: Readonly<Record<PhaseStatus, readonly PhaseStatus[]>> = {
  backlog: ["todo", "design", "cancelled"],
  todo: ["design", "planning", "implementation", "backlog", "cancelled"],
  design: ["planning", "implementation", "todo", "cancelled"],
  planning: ["implementation", "design", "cancelled"],
  implementation: ["code_review", "testing", "planning", "design", "cancelled"],
  code_review: ["testing", "documentation", "implementation", "cancelled"],
  testing: ["documentation", "done", "implementation", "code_review", "cancelled"],
  documentation: ["done", "testing", "cancelled"],
  done: ["backlog", "todo"],
  cancelled: ["backlog", "todo"],
};

/**
 * May a card in `from` (a read-model state, possibly a legacy engine lane) be
 * dropped into the `to` phase column? A no-op (same column) is NOT a move. An
 * unknown origin can never move (fail-closed).
 */
export function canTransition(from: string, to: PhaseStatus): boolean {
  const origin = phaseOf(from);
  if (origin == null) return false;
  if (origin === to) return false;
  return ALLOWED_DROPS[origin].includes(to);
}

/** The allowed drop targets for a card's current state — drives the quick-move menu. */
export function allowedTargets(from: string): readonly PhaseStatus[] {
  const origin = phaseOf(from);
  return origin == null ? [] : ALLOWED_DROPS[origin];
}
