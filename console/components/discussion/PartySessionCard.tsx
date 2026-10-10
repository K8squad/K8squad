// PartySessionCard — the ISI-5589 WS-E framing for a party-mode debate
// (ISI-5569 / ADR-0027). It renders the coherent session container around the
// bare transcript: a header with the lifecycle phase, a live ROUND meter and a
// PAID-RUN budget meter (from GET …/party-sessions/active), and an
// END-OF-SESSION takeaways summary.
//
// Two data paths, by what the WS-B wire exposes:
//   - LIVE session object present → full framing: round `n/N`, paid-run meter,
//     voices/round, phase banner (and, if the fetched session is already
//     terminal, the full takeaways).
//   - No live session but a `party_start` opener is in the transcript → the
//     debate ended and its session object is no longer reachable (`/active`
//     filters phase='active'; no get-by-id/list, no session-state SSE in WS-B).
//     We render a DEGRADED takeaways derived from the transcript alone: the agent
//     voices that contributed, without the round/budget tallies the wire drops.
//
// Turn SEQUENCE (ISI-5640 C4, ADR-0031): since ISI-5638 dispatches party voices
// strictly sequentially (one in flight, the next only after the prior settles),
// the card renders the session's agent turns IN ORDER — each voice's turn shown
// after the prior one, reflecting the reactive dispatch order. The order is
// inherent in the transcript's `createdAt`; the ISI-5616 round wire (when present)
// additionally tags each turn with its round. See the party.ts `partyTurns` note.

import type { Message, PartySession } from "@/lib/discussion/types";
import {
  deriveTakeaways,
  endedTakeaways,
  partySessionView,
  partyTurns,
  type PartyTakeaways,
  type PartyTurn,
} from "@/lib/discussion/party";

export interface PartySessionCardProps {
  /** The live session from `/party-sessions/active`, or null when none is active. */
  session?: PartySession | null;
  /** The `party_start` opener message in the transcript, if the thread has one. */
  opener?: Message | null;
  /** The thread transcript — the source for the client-derived takeaways voices. */
  messages: readonly Message[];
}

function Takeaways({ takeaways }: { takeaways: PartyTakeaways }) {
  return (
    <div className="ksq-party__takeaways" data-testid="party-takeaways">
      <h4 className="ksq-party__takeaways-title">Takeaways</h4>
      <p className="ksq-party__takeaways-outcome" data-testid="party-outcome">
        {takeaways.phaseLabel}
        {takeaways.terminalReasonKnown &&
        takeaways.roundsCompleted != null &&
        takeaways.paidRunBudget != null ? (
          <>
            {" · "}
            {takeaways.roundsCompleted} round
            {takeaways.roundsCompleted === 1 ? "" : "s"}
            {" · "}
            {takeaways.paidRunsUsed}/{takeaways.paidRunBudget} paid runs
          </>
        ) : null}
      </p>
      {takeaways.voices.length > 0 ? (
        <ul className="ksq-party__voices" data-testid="party-voices">
          {takeaways.voices.map((v) => (
            <li key={v.agentId} className="ksq-party__voice">
              <span className="ksq-party__voice-name">{v.principal}</span>
              <span
                className="ksq-chip ksq-party__voice-count"
                title={`${v.contributions} contribution${
                  v.contributions === 1 ? "" : "s"
                }`}
              >
                {v.contributions}
              </span>
            </li>
          ))}
        </ul>
      ) : (
        <p className="ksq-party__voices-empty">No agent voices contributed.</p>
      )}
    </div>
  );
}

function TurnSequence({ turns }: { turns: readonly PartyTurn[] }) {
  if (turns.length === 0) return null;
  return (
    <ol className="ksq-party__turns" data-testid="party-turns">
      {turns.map((t) => (
        <li
          key={t.messageId}
          className="ksq-party__turn"
          data-turn={t.turnIndex}
          data-round={t.round ?? undefined}
        >
          <span className="ksq-chip ksq-party__turn-idx" aria-hidden>
            {t.turnIndex}
          </span>
          <span className="ksq-party__turn-name">{t.principal}</span>
          {t.round != null ? (
            <span className="ksq-party__turn-round">round {t.round + 1}</span>
          ) : null}
        </li>
      ))}
    </ol>
  );
}

export function PartySessionCard({
  session,
  opener,
  messages,
}: PartySessionCardProps) {
  // Nothing to frame: no active session and no opener in the transcript.
  if (!session && !opener) return null;

  // Degraded post-close path: a debate was started (opener present) but its
  // session object is no longer reachable. Transcript-only takeaways.
  if (!session) {
    return (
      <section
        className="ksq-party ksq-party--ended"
        data-testid="party-session"
        data-phase="ended"
        data-terminal="true"
      >
        <header className="ksq-party__head">
          <span className="ksq-chip ksq-party__badge">Party</span>
          <span className="ksq-party__phase" data-testid="party-phase">
            Debate ended
          </span>
        </header>
        <TurnSequence
          turns={partyTurns(
            { openedAt: opener!.createdAt, closedAt: null },
            messages,
          )}
        />
        <Takeaways takeaways={endedTakeaways(opener!, messages)} />
      </section>
    );
  }

  const view = partySessionView(session);
  const takeaways = view.isTerminal ? deriveTakeaways(session, messages) : null;
  const turns = partyTurns(session, messages);

  return (
    <section
      className="ksq-party"
      data-testid="party-session"
      data-phase={view.phase}
      data-terminal={view.isTerminal}
    >
      <header className="ksq-party__head">
        <span className="ksq-chip ksq-party__badge">Party</span>
        <span className="ksq-party__phase" data-testid="party-phase">
          {view.phaseLabel}
        </span>
      </header>

      <div className="ksq-party__meters">
        <div className="ksq-party__meter" data-testid="party-rounds">
          <span className="ksq-party__meter-label">Rounds</span>
          <span className="ksq-party__meter-val">
            {view.currentRound} / {view.maxRounds}
          </span>
          <progress
            className="ksq-party__bar"
            max={1}
            value={view.roundProgress}
            aria-label={`Round ${view.currentRound} of ${view.maxRounds}`}
          />
        </div>

        <div className="ksq-party__meter" data-testid="party-runs">
          <span className="ksq-party__meter-label">Paid runs</span>
          <span
            className="ksq-party__meter-val"
            data-exhausted={view.isExhausted}
          >
            {view.paidRunsUsed} / {view.paidRunBudget}
          </span>
          <progress
            className="ksq-party__bar"
            max={1}
            value={view.paidRunProgress}
            aria-label={`${view.paidRunsUsed} of ${view.paidRunBudget} paid runs used`}
          />
          <span className="ksq-party__hint">
            {view.remainingPaidRuns} left · up to {view.maxVoicesPerRound}{" "}
            voices/round
          </span>
        </div>
      </div>

      <TurnSequence turns={turns} />

      {takeaways ? <Takeaways takeaways={takeaways} /> : null}
    </section>
  );
}
