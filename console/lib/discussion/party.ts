// Party-session projection (ISI-5589 WS-E, consuming ISI-5585 WS-B / ADR-0027).
//
// A party session is a facilitated, bounded, multi-round team debate (ISI-5569).
// WS-B (PR #822) landed the server-owned session model: the opener is a
// kind='party_start' message, and the live session state — round counter, hard
// cost budgets, paid-run tally, lifecycle `phase` — is fetched from
// `GET …/party-sessions/active` (NOT carried per-message: WS-B puts no session id
// or round number on the message wire).
//
// This module is the PURE, transport-free projection the console renders from:
//   1. a session's budget/round/phase into a display view (the progress + budget
//      meter), and
//   2. an END-OF-SESSION takeaways summary, DERIVED CLIENT-SIDE — WS-B exposes no
//      backend takeaways/summary concept (the only machine-readable end signals
//      are the terminal `phase` and `closedAt`), so the card is assembled from the
//      session + the thread transcript.
//
// SCOPE NOTE: per-ROUND grouping of voice messages is now wire-supported — ISI-5616
// (ADR-0027 addendum) added read-derived `partySessionId` / `partyRound` /
// `partyRoundKind` to the message DTO (see `types.ts#Message`), so voices group on a
// stable server-provided round number rather than the old thread-window heuristic.
// The thin grouping wire-up + vitest that consume those fields land in ISI-5617; the
// session-derived helpers below (budget meter, takeaways card) are unchanged.

import type { Message, PartyPhase, PartySession } from "./types";
import { KIND_PARTY_START } from "./types";

/** True when a message is the server-stamped party-session opener (ISI-5585). */
export function isPartyStart(m: Pick<Message, "kind">): boolean {
  return m.kind === KIND_PARTY_START;
}

/** True for the terminal phases (`closed`/`converged`/`budget_exhausted`). */
export function isTerminalPhase(phase: PartyPhase): boolean {
  return phase !== "active";
}

/** Short human label for a phase (banner / takeaways heading). */
export function phaseLabel(phase: PartyPhase): string {
  switch (phase) {
    case "active":
      return "Debate in progress";
    case "converged":
      return "Converged — facilitator stopped early";
    case "budget_exhausted":
      return "Stopped — paid-run budget exhausted";
    case "closed":
    default:
      return "Debate closed";
  }
}

/** The display view of a live/terminal session's progress + budget meter. */
export interface PartySessionView {
  /** Raw server round counter (0-based, advanced on run-settle). */
  round: number;
  maxRounds: number;
  /**
   * 1-based current round for display, clamped to `maxRounds`. While active and
   * the server counter is 0, this reads "1" (the opening round). Terminal
   * sessions report the number of rounds reached (`min(round, maxRounds)`).
   */
  currentRound: number;
  /** `currentRound / maxRounds` clamped to [0,1] — the round progress fraction. */
  roundProgress: number;
  maxVoicesPerRound: number;
  paidRunsUsed: number;
  paidRunBudget: number;
  /** `max(0, paidRunBudget - paidRunsUsed)` — never negative. */
  remainingPaidRuns: number;
  /** `paidRunsUsed / paidRunBudget` clamped to [0,1] — the paid-run meter fill. */
  paidRunProgress: number;
  phase: PartyPhase;
  phaseLabel: string;
  isActive: boolean;
  isTerminal: boolean;
  /** Terminal specifically via the hard paid-run ceiling. */
  isExhausted: boolean;
}

function clamp01(n: number): number {
  if (!Number.isFinite(n) || n <= 0) return 0;
  return n >= 1 ? 1 : n;
}

/** Project a session into its display view. Pure. */
export function partySessionView(session: PartySession): PartySessionView {
  const { round, phase, paidRunsUsed } = session;
  const maxRounds = Math.max(0, session.budget.maxRounds);
  const paidRunBudget = Math.max(0, session.budget.paidRunBudget);
  const active = phase === "active";
  // While active, the server counter is the number of SETTLED rounds, so the
  // round underway is counter+1; terminal sessions report rounds reached.
  const currentRound = active
    ? Math.min(round + 1, maxRounds || round + 1)
    : Math.min(round, maxRounds || round);
  const remainingPaidRuns = Math.max(0, paidRunBudget - paidRunsUsed);
  return {
    round,
    maxRounds,
    currentRound,
    roundProgress: maxRounds > 0 ? clamp01(currentRound / maxRounds) : 0,
    maxVoicesPerRound: Math.max(0, session.budget.maxVoicesPerRound),
    paidRunsUsed: Math.max(0, paidRunsUsed),
    paidRunBudget,
    remainingPaidRuns,
    paidRunProgress: paidRunBudget > 0 ? clamp01(paidRunsUsed / paidRunBudget) : 0,
    phase,
    phaseLabel: phaseLabel(phase),
    isActive: active,
    isTerminal: isTerminalPhase(phase),
    isExhausted: phase === "budget_exhausted",
  };
}

/** One participating voice in the session's takeaways (an agent author). */
export interface PartyVoice {
  /** Display identity (the `authorPrincipal` badge label). */
  principal: string;
  /** The author agent id (present ⇒ agent, the voice filter). */
  agentId: string;
  /** Count of this voice's contributions within the session window. */
  contributions: number;
}

/** The client-derived end-of-session summary card model. */
export interface PartyTakeaways {
  phaseLabel: string;
  /**
   * True when the terminal reason + budget numbers are known (derived from a
   * fetched session object). False when derived from the transcript alone —
   * `GET /party-sessions/active` filters `phase='active'`, so a CLOSED session's
   * object is unreachable (no get-by-id/list endpoint, no session-state SSE in
   * WS-B scope); the console can still show the voices from the transcript but
   * not the round/budget tallies or the exact terminal phase. See ISI-5589
   * follow-up (backend gap).
   */
  terminalReasonKnown: boolean;
  /** Number of rounds reached — present only when `terminalReasonKnown`. */
  roundsCompleted?: number;
  paidRunsUsed?: number;
  paidRunBudget?: number;
  /** Distinct agent voices that contributed, most-active first then by name. */
  voices: PartyVoice[];
  closedAt?: string | null;
}

/**
 * The time window a message must fall in to count toward a session: from the
 * session open to its close (or now, for a still-active session). Correlation is
 * by thread window only — WS-B carries no session id on messages (see module
 * header). A missing/odd timestamp is treated as out-of-window (never counted).
 */
function inSessionWindow(
  m: Pick<Message, "createdAt">,
  openedAt: string,
  closedAt: string | null | undefined,
): boolean {
  const t = m.createdAt;
  if (typeof t !== "string" || t === "") return false;
  if (t < openedAt) return false;
  if (typeof closedAt === "string" && closedAt !== "" && t > closedAt) {
    return false;
  }
  return true;
}

/**
 * Collect the distinct AGENT voices that contributed to the session, from the
 * thread transcript, within the session window. Humans and the opener's own
 * non-agent posts are excluded (a voice is a dispatched agent turn). Ordered by
 * contribution count desc, then principal asc, for a stable render. Pure.
 */
export function partyVoices(
  session: Pick<PartySession, "openedAt" | "closedAt">,
  messages: readonly Message[],
): PartyVoice[] {
  const byAgent = new Map<string, PartyVoice>();
  for (const m of messages) {
    const agentId = m.authorAgentId;
    if (typeof agentId !== "string" || agentId === "") continue; // humans excluded
    if (!inSessionWindow(m, session.openedAt, session.closedAt)) continue;
    const existing = byAgent.get(agentId);
    if (existing) {
      existing.contributions += 1;
    } else {
      byAgent.set(agentId, {
        principal: m.authorPrincipal || agentId,
        agentId,
        contributions: 1,
      });
    }
  }
  return [...byAgent.values()].sort(
    (a, b) =>
      b.contributions - a.contributions ||
      (a.principal < b.principal ? -1 : a.principal > b.principal ? 1 : 0),
  );
}

/**
 * Derive the end-of-session takeaways card from the session + the thread
 * transcript. Purely client-side (WS-B exposes no backend takeaways). The card
 * is meaningful only once the session is terminal, but the projection is defined
 * for any session so a caller can render a live preview if desired. Pure.
 */
export function deriveTakeaways(
  session: PartySession,
  messages: readonly Message[],
): PartyTakeaways {
  return {
    phaseLabel: phaseLabel(session.phase),
    terminalReasonKnown: true,
    roundsCompleted: session.round,
    paidRunsUsed: Math.max(0, session.paidRunsUsed),
    paidRunBudget: Math.max(0, session.budget.paidRunBudget),
    voices: partyVoices(session, messages),
    closedAt: session.closedAt ?? null,
  };
}

/**
 * Derive a DEGRADED end-of-session takeaways from the TRANSCRIPT ALONE, keyed off
 * the `party_start` opener — the post-close fallback when no session object is
 * reachable (`/active` is `phase='active'`-only; see {@link PartyTakeaways}).
 * Voices are the agent authors from the opener onward; round/budget numbers are
 * omitted (`terminalReasonKnown=false`) because the wire does not expose them
 * after close. Pure.
 */
export function endedTakeaways(
  opener: Pick<Message, "createdAt">,
  messages: readonly Message[],
): PartyTakeaways {
  return {
    phaseLabel: "Debate ended",
    terminalReasonKnown: false,
    voices: partyVoices({ openedAt: opener.createdAt, closedAt: null }, messages),
    closedAt: null,
  };
}
