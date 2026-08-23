// lib/agents/status.ts — the DERIVED agent-status mapping (story 8.10).
//
// The org diagram paints a four-value legibility bucket (idle / running / blocked / paused). The
// authoritative state is the Agent's current `Run.status.phase` (§8 seven-value machine) + the
// `Paused` condition reason (Epic 7.4 / story 3.7). This module is the SINGLE, pure derivation —
// the read-model already derives it server-side; this mirrors that mapping so the client can also
// re-derive from a live SSE phase update (8.10 "status updates live over SSE") without a refetch.

import type { AgentStatus, RunPhase } from "./types";

/**
 * Map a Run phase (+ optional paused sub-reason) to the org-diagram status bucket.
 *
 *  - No current Run / terminal phase (Succeeded/Failed/Cancelled) → `idle` (the agent is free).
 *  - `Running` → `running`.
 *  - `Paused` → `paused` (the credential/rate-limit wait; auto-resumes, story 7.4/3.7).
 *  - `Pending`/`Claiming` → `blocked` (admitted but not progressing — waiting on a sandbox/claim);
 *    a `blocked` derivation is also produced when the read-model flags a blocked condition.
 */
export function deriveAgentStatus(
  phase: RunPhase | null | undefined,
  opts?: { blockedCondition?: boolean },
): AgentStatus {
  if (opts?.blockedCondition) return "blocked";
  switch (phase) {
    case "Running":
      return "running";
    case "Paused":
      return "paused";
    case "Pending":
    case "Claiming":
      return "blocked";
    case "Succeeded":
    case "Failed":
    case "Cancelled":
    default:
      return "idle";
  }
}

/** Human-facing label for a status chip (with the paused sub-reason when present, story 7.6). */
export function statusLabel(
  status: AgentStatus,
  pausedReason?: "credential" | "rate_limited" | null,
): string {
  if (status === "paused" && pausedReason) {
    return pausedReason === "rate_limited"
      ? "paused: rate-limited"
      : "paused: credential";
  }
  return status;
}
