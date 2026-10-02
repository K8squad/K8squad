"use client";

// components/agents/roster/StateViews.tsx — the empty / loading / error surfaces for the
// Agents & Team redesign (ISI-5362 / S5; mockups ISI-5306 Frame 04).
//
// Every terminal state renders an honest, legible surface — never a spinner-only blank or
// internal story-reference copy (the SessionTeamOrg discipline). The loading state is a shimmer
// skeleton that mirrors the rail + detail two-pane layout so the page does not reflow when data
// lands. These are read-only, presentational, and theme-invariant (globals.css tokens only).

/** Shimmer skeleton mirroring the rail + detail panes — shown while the four fleet lists load. */
export function RosterLoading() {
  return (
    <div className="roster-skeleton" aria-busy="true" aria-label="Loading agents and teams">
      <div className="roster-skeleton__rail">
        {Array.from({ length: 7 }).map((_, i) => (
          <div key={i} className="roster-skeleton__line" data-indent={i % 3} />
        ))}
      </div>
      <div className="roster-skeleton__detail">
        <div className="roster-skeleton__line roster-skeleton__line--title" />
        <div className="roster-skeleton__card" />
        <div className="roster-skeleton__card" />
      </div>
    </div>
  );
}

/** Non-recoverable load failure for the whole surface — honest, with a retry affordance. */
export function RosterError({ onRetry }: { onRetry: () => void }) {
  return (
    <div className="roster-state roster-state--error" role="status">
      <p className="roster-state__title">Couldn’t load your agents &amp; team</p>
      <p className="muted">
        The roster read didn’t come back. This is usually transient — try again.
      </p>
      <button type="button" className="btn" onClick={onRetry}>
        Retry
      </button>
    </div>
  );
}

/** No teams, roles, or skills resolved for this session — the honest empty roster. */
export function RosterEmpty() {
  return (
    <div className="roster-state roster-state--empty" role="status">
      <p className="roster-state__title">No agents or teams yet</p>
      <p className="muted">
        Once a Team, Role, or Skill is composed for your session, it appears here.
      </p>
    </div>
  );
}

/** The right pane before any node is selected — a quiet prompt, not an error. */
export function DetailPlaceholder() {
  return (
    <div className="roster-state roster-state--placeholder" role="status">
      <p className="muted">Select a team, agent, role, or skill to see its detail.</p>
    </div>
  );
}
