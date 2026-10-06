"use client";

// Roster — the room's agent roster with presence (ISI-4929, plan §4.5:
// `online / working / offline`). A pure projection of the Team read model
// (`lib/agents` org shapes): the presence dot folds the four-value agent
// status bucket (idle/running/blocked/paused) onto the roster's three-value
// bucket, and the sub-label keeps the known status text legible. v1 degrade:
// no last-seen timestamp exists on the stream, so an unknown status renders
// offline with a dash rather than inventing a time.
//
// ISI-5527 (parent ISI-5520): the row now visibly flips idle → queued → running
// as the agent's run starts/stops, driven by the ONE live-runs source this
// console has — GET /api/squad/overview (RoomClient polls it; `liveRuns` is the
// per-agent fold, lib/overview/liveRuns.ts#liveRunsByAgent). A live run paints
// the shipped live-run idiom — the blue pulse dot + a "running"/"queued" chip,
// a blue row tint + ring, and a `working a run · <ticket ref>` sub-label — so
// the change reads at a glance; idle stays the plain presence row (byte-identical
// to ISI-4929). The transition is announced via an aria-live=polite region, the
// same mechanism WorkingIndicator uses (ISI-5174). The `liveRuns` map is optional:
// an unwired/absent overview degrades the panel to the plain presence render.
//
// Display-only legibility surface — no custody verb rides this panel.

import {
  presenceFromStatus,
  presenceSublabel,
  type Presence,
} from "@/lib/discussion/presence";
import type { AgentRunPresence } from "@/lib/overview/liveRuns";

/** One roster row: what the room needs from a Team agent. */
export interface RosterAgent {
  id: string;
  name: string;
  /** The four-value agent status bucket (idle/running/blocked/paused). */
  status?: string;
}

export interface RosterProps {
  agents: readonly RosterAgent[];
  /**
   * Per-agent live-run presence, keyed by the agent's roster id (which mirrors
   * the @-mention name). Derived from the squad/overview feed
   * (lib/overview/liveRuns.ts#liveRunsByAgent). An agent absent from the map has
   * no live run → the row renders idle. Optional: omit to degrade to the plain
   * presence roster (no overview wired).
   */
  liveRuns?: Record<string, AgentRunPresence>;
}

/** The sub-label a running/queued row shows: the honest run state + ticket ref. */
function runSublabel(run: AgentRunPresence): string {
  if (run.state === "queued") return "queued";
  return run.workItem ? `working a run · ${run.workItem}` : "working a run";
}

export function Roster({ agents, liveRuns }: RosterProps) {
  // The agents the screen reader should hear about as the set changes (ISI-5174
  // aria-live mechanism): name + honest state, in roster order. Recomputed each
  // render so a start/stop transition updates the polite region.
  const announce = agents
    .map((a) => {
      const run = liveRuns?.[a.id];
      if (!run) return null;
      return `${a.name} is ${run.state}`;
    })
    .filter(Boolean)
    .join(". ");

  return (
    <aside
      className="ksq-roster"
      data-testid="roster"
      aria-label="Team roster"
    >
      <h2 className="ksq-roster__title">Roster</h2>
      {/* Polite live region: announces idle⇄running/queued transitions without
          stealing focus (same mechanism as WorkingIndicator, ISI-5174). */}
      <p
        className="sr-only"
        role="status"
        aria-live="polite"
        data-testid="roster-live-region"
      >
        {announce}
      </p>
      {agents.length === 0 ? (
        <p className="ksq-roster__empty" data-testid="roster-empty">
          No agents on this team yet.
        </p>
      ) : (
        <ul className="ksq-roster__list">
          {agents.map((a) => {
            const run = liveRuns?.[a.id];
            const runState: "idle" | "queued" | "running" =
              run?.state ?? "idle";
            const presence: Presence = presenceFromStatus(a.status);
            return (
              <li
                key={a.id}
                className={`ksq-roster__agent${
                  run ? ` ksq-roster__agent--${run.state}` : ""
                }`}
                data-testid="roster-agent"
                data-agent-id={a.id}
                data-presence={presence}
                data-run-state={runState}
              >
                {run ? (
                  <>
                    {/* Blue dot: pulses while running, static while queued —
                        reuses the shipped pulse motion (no new token). */}
                    <span
                      className={`ksq-roster__livedot ksq-roster__livedot--${run.state}`}
                      aria-hidden="true"
                    />
                    <span className="ksq-roster__name">{a.name}</span>
                    <span
                      className={`ksq-roster__chip ksq-roster__chip--${run.state}`}
                      data-testid="roster-run-chip"
                    >
                      {run.state}
                    </span>
                    <span className="ksq-roster__status" data-testid="roster-run-sublabel">
                      {runSublabel(run)}
                    </span>
                  </>
                ) : (
                  <>
                    <span
                      className={`ksq-roster__dot ksq-roster__dot--${presence}`}
                      aria-hidden="true"
                    />
                    <span className="ksq-roster__name">{a.name}</span>
                    <span className="ksq-roster__status">
                      {presenceSublabel(a.status)}
                    </span>
                  </>
                )}
              </li>
            );
          })}
        </ul>
      )}
      <p className="ksq-roster__hint">Presence degrades to status-only (v1).</p>
    </aside>
  );
}
