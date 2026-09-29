"use client";

// TicketWorkingIndicator — the ticket-view "an agent is working…" pill (ISI-5196,
// S5 of ISI-5185). The direct port of the discussion room's WorkingIndicator
// (ISI-5174): same `ksq-working*` markup, same a11y (aria-live=polite + role=status
// so a screen reader announces the transition without stealing focus), same three
// phases (working → replied | failed). Two ticket-specific differences, both by
// design (see lib/tickets/working.ts):
//   - driven off the ticket's SINGLE per-run SSE bus (useDispatchWatch), no second
//     stream and no polling (ISI-5174 AC4 / WS-B AC4, Mock 2);
//   - the `working` label carries the CURRENT step verb ("… is editing MessageItem
//     .tsx…") — a pure label projection over the live `thinking` rows; the phase
//     stays `working`.
//
// The animated dots are display-only; reduced-motion holds them static (the
// `ksq-working*` rules — mirrored into tickets.css because the ticket route does
// not load discussion.css — carry the prefers-reduced-motion guard).
//
// PURE PRESENTATIONAL: it owns no state and no signals — the parent hands it the
// composed `TicketWorkingView`; we only paint it.

import type { TicketWorkingView } from "@/lib/tickets/working";

export function TicketWorkingIndicator({
  view,
}: {
  view: TicketWorkingView | null;
}) {
  if (!view) return null;
  const { phase, label, agentName } = view;
  return (
    <ul
      className="ksq-working-list"
      data-testid="ticket-working-list"
      aria-live="polite"
      role="status"
    >
      {phase === "replied" ? (
        <li
          className="ksq-working ksq-working--replied"
          data-testid="ticket-working-indicator"
          data-phase="replied"
          data-agent={agentName}
        >
          <span className="ksq-working__check" aria-hidden="true">
            ✓
          </span>
          <span className="ksq-working__label">{label}</span>
        </li>
      ) : phase === "failed" ? (
        <li
          className="ksq-working ksq-working--failed"
          data-testid="ticket-working-indicator"
          data-phase="failed"
          data-agent={agentName}
        >
          <span className="ksq-working__warn" aria-hidden="true">
            !
          </span>
          <span className="ksq-working__label">{label}</span>
        </li>
      ) : (
        <li
          className="ksq-working ksq-working--active"
          data-testid="ticket-working-indicator"
          data-phase="working"
          data-agent={agentName}
        >
          <span className="ksq-working__dots" aria-hidden="true">
            <span className="ksq-working__dot" />
            <span className="ksq-working__dot" />
            <span className="ksq-working__dot" />
          </span>
          <span className="ksq-working__label">{label}</span>
        </li>
      )}
    </ul>
  );
}
