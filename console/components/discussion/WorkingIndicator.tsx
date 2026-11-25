"use client";

// WorkingIndicator — the live "an agent is working…" affordance (ISI-5174).
//
// Rendered beneath a message the human just posted, one row per agent the post
// @-mentioned (the DispatchWatch entries the room seeds + resolves in
// lib/discussion/working.ts). It is a PURE projection of that state:
//   - working  → "{agent} is working…" with an animated typing indicator, so the
//                room visibly does something while the background run is live;
//                a provably QUEUED run (ISI-7772) renders its own honest copy:
//                "{agent} is queued behind the endpoint gate…";
//   - replied  → a brief "{agent} replied" confirmation (the reply itself lands
//                as its own message over the SSE channel);
//   - waiting  → "{agent} hasn't replied yet" — the honest timed-out seat
//                (ISI-7772: a timeout is not proof of failure, and a
//                never-dispatched agent must not read as a failed one);
//   - failed   → "{agent} could not respond" — reserved for surfaces that can
//                PROVE the run failed (the ticket ladder's real run state).
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
  // ISI-7772: a provably queued run renders its real state — waiting for an
  // endpoint slot behind the gate — instead of the generic working label.
  if (watch.phase === "working" && watch.live === "queued") {
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
        <span className="ksq-working__label">
          {watch.agentName} is queued behind the endpoint gate…
        </span>
      </li>
    );
  }
  // ISI-7772: the timed-out seat tells the truth — no reply yet — instead of
  // claiming the agent "could not respond" without evidence.
  if (watch.phase === "waiting") {
    return (
      <li
        className="ksq-working ksq-working--waiting"
        data-testid="working-indicator"
        data-phase="waiting"
        data-agent={watch.agentName}
      >
        <span className="ksq-working__wait" aria-hidden="true">
          …
        </span>
        <span className="ksq-working__label">
          {watch.agentName} hasn&apos;t replied yet
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
