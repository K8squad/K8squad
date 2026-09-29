"use client";

// WorkingIndicator — the live "an agent is working…" affordance (ISI-5174).
//
// Rendered beneath a message the human just posted, one row per agent the post
// @-mentioned (the DispatchWatch entries the room seeds + resolves in
// lib/discussion/working.ts). It is a PURE projection of that state:
//   - working  → "{agent} is working…" with an animated typing indicator, so the
//                room visibly does something while the background run is live;
//   - replied  → a brief "{agent} replied" confirmation (the reply itself lands
//                as its own message over the SSE channel);
//   - failed   → "{agent} could not respond" instead of silence (AC3).
//
// aria-live=polite + role=status so a screen reader announces the transition
// without stealing focus; the animation is display-only.

import type { DispatchWatch } from "@/lib/discussion/working";

function Row({ watch }: { watch: DispatchWatch }) {
  if (watch.phase === "replied") {
    return (
      <li
        className="ksq-working ksq-working--replied"
        data-testid="working-indicator"
        data-phase="replied"
        data-agent={watch.agentName}
      >
        <span className="ksq-working__check" aria-hidden="true">
          ✓
        </span>
        <span className="ksq-working__label">{watch.agentName} replied</span>
      </li>
    );
  }
  if (watch.phase === "failed") {
    return (
      <li
        className="ksq-working ksq-working--failed"
        data-testid="working-indicator"
        data-phase="failed"
        data-agent={watch.agentName}
      >
        <span className="ksq-working__warn" aria-hidden="true">
          !
        </span>
        <span className="ksq-working__label">
          {watch.agentName} could not respond
        </span>
      </li>
    );
  }
  return (
    <li
      className="ksq-working ksq-working--active"
      data-testid="working-indicator"
      data-phase="working"
      data-agent={watch.agentName}
    >
      <span className="ksq-working__dots" aria-hidden="true">
        <span className="ksq-working__dot" />
        <span className="ksq-working__dot" />
        <span className="ksq-working__dot" />
      </span>
      <span className="ksq-working__label">{watch.agentName} is working…</span>
    </li>
  );
}

/** Render the working/replied/failed rows for one message's dispatch watches. */
export function WorkingIndicators({
  watches,
}: {
  watches?: readonly DispatchWatch[];
}) {
  if (!watches || watches.length === 0) return null;
  return (
    <ul
      className="ksq-working-list"
      data-testid="working-list"
      aria-live="polite"
      role="status"
    >
      {watches.map((w) => (
        <Row key={w.key} watch={w} />
      ))}
    </ul>
  );
}
