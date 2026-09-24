"use client";

// components/tickets/RailAssigneeChip.tsx — ISI-4882 (S4 of ISI-4853): the rail assignee chip,
// upgraded to track the honest dispatch ladder while a run is on its way.
//
// Design SoT: ksquad/docs/bmad/ux/isi-4853-run-active-signal/DESIGN-SPEC-ISI-4853.md (NAS),
// story breakdown §5.5 + §6 S4. Consumes the S1 hook (`useDispatchWatch`, ISI-4879).
//
// Grammar reuse (NOT re-invention): the label follows the `localRunBadge` grammar
// (`GitHubIssuesKanban.tsx:124`) — "<agent> <verb>" — extended from that 3-state badge to the
// 4-step ladder: queued → picking up… → working… → finished. The tone is a verbatim reuse of the
// existing phase-status hues (globals.css `--status-*`), rendered through the existing
// `.ksq-chip--phase` dot+tint chip (tickets.css:127). Zero new colours / radii / tokens (AC).
//
// Honesty guard (ADR-0013 / ISI-4760, inherited from S1): "working…" is reachable ONLY when the run
// is genuinely `Running`; before that the chip says "queued"/"picking up…", never more than the data
// proves. When no dispatch is in flight the chip renders the caller's static fallback unchanged.

import type React from "react";
import type { DispatchState, DispatchWatch } from "@/lib/tickets/useDispatchWatch";

/** The chip view derived from the ladder: the localRunBadge-grammar label + a verbatim status hue. */
export interface AssigneeChipView {
  label: string;
  /** An existing globals.css status-hue token — fed to `--phase-hue` on `.ksq-chip--phase`. */
  hue: string;
  state: DispatchState;
}

/** The ladder verb, in localRunBadge grammar (queued → picking up… → working… → finished/failed). */
function chipVerb(state: DispatchState): string {
  switch (state) {
    case "queued":
      return "queued";
    case "picking_up":
      return "picking up…";
    case "working":
      return "working…";
    case "succeeded":
      return "finished";
    case "failed":
      return "failed";
  }
}

/** The ladder tone → an EXISTING status hue token (§3). No new tokens are introduced. */
function chipHue(state: DispatchState): string {
  switch (state) {
    case "queued":
      return "var(--status-idle)"; // idle grey #64748b
    case "picking_up":
      return "var(--status-paused)"; // paused amber #fbbf24
    case "working":
    case "succeeded":
      return "var(--status-running)"; // running green #34d399
    case "failed":
      return "var(--status-blocked)"; // blocked rose #fb7185
  }
}

/**
 * Pure projection: the tri-state chip for a dispatched `agent` given the S1 ladder `watch`, or null
 * when there is nothing to show (no agent, or no dispatch in flight ⇒ caller falls back to its static
 * chip). Kept pure + exported so the render test can drive all four states without mounting the hook.
 */
export function railAssigneeChip(
  agent: string,
  watch: DispatchWatch | null,
): AssigneeChipView | null {
  const name = agent.trim();
  if (!name || !watch) return null;
  return {
    label: `${name} ${chipVerb(watch.state)}`,
    hue: chipHue(watch.state),
    state: watch.state,
  };
}

/**
 * Presentational chip — pure props, no data fetching, so tests render it directly across every ladder
 * state and the idle fallback. Renders the ladder chip when `railAssigneeChip` yields one, else the
 * caller's `fallback` (today's static assignee markup) verbatim.
 */
export function AssigneeChipView({
  agent,
  watch,
  fallback,
}: {
  agent: string | null;
  watch: DispatchWatch | null;
  fallback: React.ReactNode;
}) {
  const chip = railAssigneeChip(agent ?? "", watch);
  if (!chip) return <>{fallback}</>;
  return (
    <span
      className="ksq-chip ksq-chip--phase"
      data-testid="detail-assignee-chip"
      data-state={chip.state}
      role="status"
      aria-label={`Dispatch status: ${chip.label}`}
      style={{ ["--phase-hue" as string]: chip.hue }}
    >
      <span className="ksq-phase-dot" aria-hidden="true" />
      {chip.label}
    </span>
  );
}

// NOTE: the rail chip is driven by the parent's SINGLE `useDispatchWatch` (TicketDetail seeds it only
// on a real dispatch 200 — composer OR rail select) and passed in as `watch`. It is deliberately NOT a
// self-mounting hook keyed on `requestedAgent`: a passively-viewed ticket has a stamped requested agent
// but nothing in flight, and must show its static fallback, never a fabricated "queued" chip.
