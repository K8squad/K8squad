"use client";

// Roster — the room's agent roster with presence (ISI-4929, plan §4.5:
// `online / working / offline`). A pure projection of the Team read model
// (`lib/agents` org shapes): the presence dot folds the four-value agent
// status bucket (idle/running/blocked/paused) onto the roster's three-value
// bucket, and the sub-label keeps the known status text legible. v1 degrade:
// no last-seen timestamp exists on the stream, so an unknown status renders
// offline with a dash rather than inventing a time.
//
// Display-only legibility surface — no custody verb rides this panel.

import {
  presenceFromStatus,
  presenceSublabel,
  type Presence,
} from "@/lib/discussion/presence";

/** One roster row: what the room needs from a Team agent. */
export interface RosterAgent {
  id: string;
  name: string;
  /** The four-value agent status bucket (idle/running/blocked/paused). */
  status?: string;
}

export function Roster({ agents }: { agents: readonly RosterAgent[] }) {
  return (
    <aside
      className="ksq-roster"
      data-testid="roster"
      aria-label="Team roster"
    >
      <h2 className="ksq-roster__title">Roster</h2>
      {agents.length === 0 ? (
        <p className="ksq-roster__empty" data-testid="roster-empty">
          No agents on this team yet.
        </p>
      ) : (
        <ul className="ksq-roster__list">
          {agents.map((a) => {
            const presence: Presence = presenceFromStatus(a.status);
            return (
              <li
                key={a.id}
                className="ksq-roster__agent"
                data-testid="roster-agent"
                data-agent-id={a.id}
                data-presence={presence}
              >
                <span
                  className={`ksq-roster__dot ksq-roster__dot--${presence}`}
                  aria-hidden="true"
                />
                <span className="ksq-roster__name">{a.name}</span>
                <span className="ksq-roster__status">
                  {presenceSublabel(a.status)}
                </span>
              </li>
            );
          })}
        </ul>
      )}
      <p className="ksq-roster__hint">Presence degrades to status-only (v1).</p>
    </aside>
  );
}
