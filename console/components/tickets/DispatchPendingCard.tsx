// components/tickets/DispatchPendingCard.tsx — ISI-4880 (S2 of ISI-4853): the placeholder card that
// fills the dead-air gap between "assign to agent" and the first live RunCommentCard.
//
// Design SoT: ksquad/docs/bmad/stories/isi-4853-run-active-signal-story-breakdown.md (§3 ladder, §5.2),
// board-locked (interaction f3d5c62e): first label = "Queued".
//
// PURE PRESENTATIONAL. It owns no state and no signals — the S1 hook (useDispatchWatch, ISI-4879)
// resolves the honest ladder and hands us `{state,label,tone,degrade}`; we only PAINT it. To guarantee
// "no layout shift when the real RunCommentCard replaces the placeholder" (AC), we render the SAME
// shell RunCommentCard uses — `<li class="ksq-activity ksq-activity--comment ksq-runcomment">` with the
// avatar spine + bubble — so the real card mutates in place at this exact slot.
//
// ZERO new colours / tokens / keyframes (AC): every tone keys a LOCKED status token from globals.css
// (§status: --status-idle/paused/running/blocked) and every motion reuses an EXISTING animation that is
// already `prefers-reduced-motion` gated — skeleton `ksq-skel-bar` (tickets.css), `.rail__spinner`
// (globals.css), and the run pulse `ksq-runcomment__dot--live` (tickets.css). So reduced-motion is
// respected for free, inherited from the reused rules.

import type { DispatchState, DispatchWatch } from "@/lib/tickets/useDispatchWatch";

/**
 * The per-state indicator (§3 "tone / motion"):
 *   queued    → static idle-grey dot (hue via the container's data-tone)
 *   picking_up→ the shared rail spinner, repainted to the locked paused hue
 *   working   → the EXISTING run pulse (ksq-runcomment__dot--live)
 *   terminal  → static dot (running-green / blocked-rose, via data-tone)
 * Every element is decorative; the live label carries the meaning (aria-hidden here).
 */
function StatusIndicator({ state }: { state: DispatchState }) {
  if (state === "picking_up") {
    return (
      <span
        className="rail__spinner ksq-dispatch-card__spinner"
        data-testid="dispatch-indicator"
        data-motion="spinner"
        aria-hidden="true"
      />
    );
  }
  if (state === "working") {
    // Reuse the run card's live pulse verbatim (green #34d399 + ksq-run-pulse, reduced-motion gated).
    return (
      <span
        className="ksq-runcomment__dot ksq-runcomment__dot--live"
        data-testid="dispatch-indicator"
        data-motion="pulse"
        aria-hidden="true"
      />
    );
  }
  // queued + terminal (succeeded/failed): a static dot; the hue comes from the container data-tone.
  return (
    <span
      className="ksq-dispatch-card__dot"
      data-testid="dispatch-indicator"
      data-motion="static"
      aria-hidden="true"
    />
  );
}

/** Honest "still waiting" copy shown ONLY while the ladder is still Queued (design §4). Terminal and
 *  advanced states carry no note — the label already tells the truth. */
function degradeNote(watch: DispatchWatch): string | null {
  if (watch.state !== "queued") return null;
  if (watch.degrade === "soft")
    return "Still waiting for the operator… this usually takes a few seconds.";
  if (watch.degrade === "stalled")
    return "Operator hasn't picked this up yet. Check the Runs screen or re-assign.";
  return null;
}

/**
 * The placeholder card. Sits at the run card's eventual slot in the Activity stream and mutates in
 * place through the ladder until the real RunCommentCard takes over (dedupe/hand-off is S3's job).
 *
 * @param watch     the S1 hook output (never null here — the parent only mounts us on an active watch)
 * @param agentName the dispatched agent's display name (header)
 * @param avatar    optional avatar glyph for the spine (matches RunCommentCard's `meta.avatar`)
 */
export function DispatchPendingCard({
  watch,
  agentName,
  avatar,
}: {
  watch: DispatchWatch;
  agentName: string;
  avatar?: string;
}) {
  const note = degradeNote(watch);
  return (
    <li
      className="ksq-activity ksq-activity--comment ksq-runcomment ksq-dispatch-card"
      data-testid="dispatch-pending-card"
      data-role="agent"
      data-state={watch.state}
      data-tone={watch.tone}
      role="status"
      aria-live="polite"
      aria-label={`Run status: ${watch.label}`}
    >
      <span className="ksq-runcomment__avatar" data-kind="agent" aria-hidden="true">
        {avatar ?? "?"}
      </span>
      <div className="ksq-runcomment__bubble">
        <div className="ksq-activity__head ksq-runcomment__head">
          <strong>{agentName}</strong>
          <span className="ksq-chip" data-role="agent">
            agent
          </span>
          <span
            className="ksq-dispatch-card__status"
            data-testid="dispatch-status"
            data-tone={watch.tone}
          >
            <StatusIndicator state={watch.state} />
            <span className="ksq-dispatch-card__label" data-testid="dispatch-label">
              {watch.label}
            </span>
          </span>
        </div>

        {/* Placeholder body — the shimmer skeleton stands in for the not-yet-arrived run content, so
            the bubble keeps a real card's footprint and nothing jumps when the run card lands. */}
        <div className="ksq-dispatch-card__skeleton" data-testid="dispatch-skeleton" aria-hidden="true">
          <span className="ksq-skel-bar" />
          <span className="ksq-skel-bar" />
        </div>

        {note && (
          <p className="ksq-dispatch-card__degrade" data-testid="dispatch-degrade">
            {note}
          </p>
        )}
      </div>
    </li>
  );
}
