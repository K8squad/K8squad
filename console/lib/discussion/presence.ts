// Roster presence projection (ISI-4929, plan §4.5 — Discussion Room v2 story 5).
//
// The roster shows agent presence as the three-value bucket `online / working /
// offline` (plan §4.5). The console's live agent status (lib/agents) derives a
// four-value legibility bucket from the Agent's current Run phase
// (idle | running | blocked | paused, §8); this module is the pure fold from
// that bucket onto the roster's three-value presence.
//
// v1 degrade: there is no last-seen timestamp on the status stream, so an
// unknown/absent status renders `offline` rather than fabricating a timestamp —
// the roster shows only what the read model actually knows.

import type { AgentStatus } from "@/lib/agents/types";

/** The roster's three-value presence bucket (plan §4.5). */
export type Presence = "online" | "working" | "offline";

/**
 * Fold the agent status bucket onto roster presence:
 *   running → working · idle → online · paused/blocked/unknown → offline
 * (paused and blocked agents are not accepting new work in the room sense;
 * their exact sub-state stays visible on the org diagram, not the roster.)
 */
export function presenceFromStatus(
  status?: AgentStatus | string | null,
): Presence {
  switch (status) {
    case "running":
      return "working";
    case "idle":
      return "online";
    default:
      return "offline";
  }
}

/** The status text rendered next to the name — the known bucket, else a dash. */
export function presenceSublabel(status?: string | null): string {
  return status && status.length > 0 ? status : "—";
}
