// lib/inbox/options.ts — WS-6.4 (ISI-7226) of the ISI-7074 comment-quality plan: the
// shared multi-option presentation contract for decision cards. Henrik 2026-11-08: a
// decision request must ALWAYS present multiple labeled options a/b/c/d... — the board
// picks one, the agent resumes with the typed answer. The backend contract
// (DecisionOption / InboxDecisionOption) carries no letter field — the letter is the
// option's index rendered as a chip, derived here so the inbox row (inline choose_one)
// and the ticket-detail DecisionCard can never disagree. WS-6.1 caps options at 9
// server-side; the number-KEY affordance is capped at 5 (one-handed a11y contract).

/** The highest option index reachable with a number key (a11y contract, WS-6.4). */
export const DECISION_NUMBER_KEY_MAX = 5;

/** The a/b/c... letter for a 0-based option index (0 -> "a"). */
export function optionLetter(index: number): string {
  return String.fromCharCode(97 + index);
}

/** True when the 0-based option index is reachable with a number key (1-5). */
export function numberKeyReaches(index: number): boolean {
  return index < DECISION_NUMBER_KEY_MAX;
}
