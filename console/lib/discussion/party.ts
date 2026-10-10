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
// ISI-5613 (WS-E follow-up) lands the two pieces WS-E could not build against the
// original WS-B wire, now that both backend gaps are on main:
//   - Gap 1 (ISI-5616): read-derived `partySessionId` / `partyRound` /
//     `partyRoundKind` on the message DTO (see `types.ts#Message`). `partyRounds`
//     below groups a thread's party messages into numbered rounds on that stable
//     server-provided round number rather than the old thread-window heuristic.
//   - Gap 2 (ISI-5617): a terminal party-session read (`GET …/party-sessions?
//     includeClosed=true`). `newestTerminalSession` picks the closed session out
//     of that list so the post-close takeaways carry the REAL phase reason + round
//     count + paid-run tally (full `deriveTakeaways`), not the degraded
//     transcript-only `endedTakeaways` fallback.

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
   * fetched session object — the live `/active` read, or the terminal session
   * from the `?includeClosed=true` list, ISI-5617). False only on the degraded
   * fallback that derives voices from the transcript alone (`endedTakeaways`),
   * used when even the terminal list is unreachable (e.g. a pre-ISI-5617
   * apiserver): the console still shows the voices but not the round/budget
   * tallies or the exact terminal phase.
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
 * Reduce a set of messages to their distinct AGENT voices, ordered by
 * contribution count desc then principal asc for a stable render. Humans (no
 * `authorAgentId`) are excluded — a voice is a dispatched agent turn. Pure; the
 * shared core of {@link partyVoices} (window-scoped) and {@link partyRounds}
 * (round-scoped).
 */
function aggregateVoices(messages: Iterable<Message>): PartyVoice[] {
  const byAgent = new Map<string, PartyVoice>();
  for (const m of messages) {
    const agentId = m.authorAgentId;
    if (typeof agentId !== "string" || agentId === "") continue; // humans excluded
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
 * Collect the distinct AGENT voices that contributed to the session, from the
 * thread transcript, within the session window. Humans and the opener's own
 * non-agent posts are excluded (a voice is a dispatched agent turn). Ordered by
 * contribution count desc, then principal asc, for a stable render. Pure.
 */
export function partyVoices(
  session: Pick<PartySession, "openedAt" | "closedAt">,
  messages: readonly Message[],
): PartyVoice[] {
  return aggregateVoices(
    (function* () {
      for (const m of messages) {
        if (inSessionWindow(m, session.openedAt, session.closedAt)) yield m;
      }
    })(),
  );
}

/**
 * One numbered round of a party session (ISI-5613 Gap 1). Built from the
 * server-stamped `partyRound` linkage, not a client heuristic:
 *   - `round` 0 / `kind` "opener" — the `party_start` opener;
 *   - `round` ≥ 1 / `kind` "round" — a facilitator post and its dispatched voice
 *     replies (all stamped with that round by the backend voice-dispatch chain).
 */
export interface PartyRoundGroup {
  /** The session these messages belong to. */
  sessionId: string;
  /** 1-based round; 0 is the opener. */
  round: number;
  /** `"opener"` | `"round"`. */
  kind: string;
  /** The messages tagged to this round, in transcript (input) order. */
  messages: Message[];
  /** The distinct agent voices in this round (facilitator + dispatched voices). */
  voices: PartyVoice[];
}

/**
 * Group a thread's party messages into numbered rounds on the server-stamped
 * `partySessionId` / `partyRound` linkage (ISI-5616, ISI-5613 Gap 1). Messages
 * without that linkage (ordinary room posts, or a thread read from a pre-ISI-5616
 * apiserver that never stamps it) are skipped — so the result is `[]` for a
 * non-party thread and degrades cleanly on an old wire. Ordered by session id
 * then round ascending; within a round, input order is preserved. Pure.
 */
export function partyRounds(messages: readonly Message[]): PartyRoundGroup[] {
  const byRound = new Map<string, PartyRoundGroup>();
  for (const m of messages) {
    const sessionId = m.partySessionId;
    const round = m.partyRound;
    if (typeof sessionId !== "string" || sessionId === "") continue;
    if (typeof round !== "number" || !Number.isFinite(round)) continue;
    const key = `${sessionId}#${round}`;
    let g = byRound.get(key);
    if (!g) {
      g = {
        sessionId,
        round,
        kind: m.partyRoundKind || (round === 0 ? "opener" : "round"),
        messages: [],
        voices: [],
      };
      byRound.set(key, g);
    }
    g.messages.push(m);
  }
  const groups = [...byRound.values()].sort((a, b) =>
    a.sessionId < b.sessionId
      ? -1
      : a.sessionId > b.sessionId
        ? 1
        : a.round - b.round,
  );
  for (const g of groups) g.voices = aggregateVoices(g.messages);
  return groups;
}

/**
 * Pick the newest TERMINAL (closed/converged/budget_exhausted) session out of a
 * newest-first session list (ISI-5617, ISI-5613 Gap 2). The list read returns
 * sessions newest-first, so the first terminal row is the most recent closed
 * debate; `null` when the list has no terminal session (e.g. a still-active
 * debate, or an empty list). Pure.
 */
export function newestTerminalSession(
  sessions: readonly PartySession[],
): PartySession | null {
  for (const s of sessions) {
    if (isTerminalPhase(s.phase)) return s;
  }
  return null;
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
