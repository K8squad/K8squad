// PartySessionCard — the ISI-5589 WS-E framing for a party-mode debate
// (ISI-5569 / ADR-0027). It renders the coherent session container around the
// bare transcript: a header with the lifecycle phase, a live ROUND meter and a
// PAID-RUN budget meter (from GET …/party-sessions/active), and an
// END-OF-SESSION takeaways summary.
//
// Two data paths, by what the wire exposes (ISI-5613 closed both WS-E gaps):
//   - A session object present — the LIVE session (`/active`) or the TERMINAL one
//     (`?includeClosed=true`, ISI-5617 Gap 2) → full framing: round `n/N`,
//     paid-run meter, phase banner, and the full takeaways once terminal (real
//     phase reason + round count + paid-run tally).
//   - No session object but a `party_start` opener is in the transcript → a
//     pre-ISI-5617 apiserver that cannot serve the terminal session: we render a
//     DEGRADED takeaways derived from the transcript alone (agent voices, no
//     round/budget tallies).
//
// Per-ROUND grouping (ISI-5613 Gap 1) IS now rendered: ISI-5616 stamps
// `partySessionId` / `partyRound` / `partyRoundKind` on the message DTO, so
// `partyRounds` groups the thread's party messages into the numbered rounds the
// card lists.

import type { Message, PartySession } from "@/lib/discussion/types";
import {
  deriveTakeaways,
  endedTakeaways,
  partyRounds,
  partySessionView,
  type PartyRoundGroup,
  type PartyTakeaways,
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

/**
 * The per-round breakdown (ISI-5613 Gap 1): each numbered facilitator round with
 * the agent voices (facilitator + dispatched voices) that spoke in it. Grouping
 * is on the server-stamped `partyRound` linkage — so it renders even on the
 * degraded post-close path (the tags survive on the transcript). The round-0
 * opener is omitted; it is the thread's topic message, already shown above.
 */
function Rounds({ rounds }: { rounds: PartyRoundGroup[] }) {
  const numbered = rounds.filter((r) => r.round >= 1);
  if (numbered.length === 0) return null;
  return (
    <div className="ksq-party__rounds" data-testid="party-round-breakdown">
      <h4 className="ksq-party__rounds-title">Rounds</h4>
      <ol className="ksq-party__round-list">
        {numbered.map((r) => (
          <li
            key={`${r.sessionId}#${r.round}`}
            className="ksq-party__round"
            data-testid="party-round"
            data-round={r.round}
          >
            <span className="ksq-party__round-label">Round {r.round}</span>
            {r.voices.length > 0 ? (
              <ul className="ksq-party__round-voices">
                {r.voices.map((v) => (
                  <li key={v.agentId} className="ksq-party__round-voice">
                    <span className="ksq-party__voice-name">{v.principal}</span>
                    <span
                      className="ksq-chip ksq-party__voice-count"
                      title={`${v.contributions} message${
                        v.contributions === 1 ? "" : "s"
                      } this round`}
                    >
                      {v.contributions}
                    </span>
                  </li>
                ))}
              </ul>
            ) : (
              <span className="ksq-party__round-empty">No voices yet.</span>
            )}
          </li>
        ))}
      </ol>
    </div>
  );
}

export function PartySessionCard({
  session,
  opener,
  messages,
}: PartySessionCardProps) {
  // Per-round grouping is derived once from the transcript's party-round tags
  // (ISI-5613 Gap 1); it is independent of whether a session object is reachable.
  const rounds = partyRounds(messages);
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
        <Rounds rounds={rounds} />
        <Takeaways takeaways={endedTakeaways(opener!, messages)} />
      </section>
    );
  }

  const view = partySessionView(session);
  const takeaways = view.isTerminal ? deriveTakeaways(session, messages) : null;

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

      <Rounds rounds={rounds} />
      {takeaways ? <Takeaways takeaways={takeaways} /> : null}
    </section>
  );
}
