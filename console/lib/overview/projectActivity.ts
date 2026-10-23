// lib/overview/projectActivity.ts — fold the fleet/Team agent-activity list (WS-1a,
// GET /api/squad/agents/activity) down to the agents working on ONE project, for the
// per-project overview's agent-card grid (ISI-5709 WS-4, design ISI-5699 mock D ④).
//
// The WS-1a read model is squad-scoped, not project-scoped: each agent carries its
// current-or-last run, and that run's in-console deep-links (workItemUrl
// `/projects/{id}/issues/{ref}` or discussionUrl `/projects/{id}/discussion`) embed the
// SAME percent-encoded project id the /projects/[projectId] routes use (agentactivity.go
// inConsoleProjectID == the FE's encodeProjectId). So we attribute an agent to this project
// by matching that deep-link prefix — no bespoke join, and no fabricated attribution: an
// agent whose run links nowhere (no work item / discussion yet) is honestly NOT claimed for
// any project rather than guessed onto this one.

import { encodeProjectId } from "@/lib/projectId";
import type {
  AgentActivity,
  AgentCurrentOrLastRun,
} from "@/lib/overview/agentActivity";

/** True when a run's work-item or discussion deep-link points into `/projects/{encoded}/…`. */
function runLinksToProject(
  run: AgentCurrentOrLastRun | null | undefined,
  encoded: string,
): boolean {
  if (!run) return false;
  const prefix = `/projects/${encoded}/`;
  return (
    (run.workItemUrl?.startsWith(prefix) ?? false) ||
    (run.discussionUrl?.startsWith(prefix) ?? false)
  );
}

/**
 * The agents attributed to `projectId`, order-preserved from the server (running-first by the
 * WS-1a projection). Only agents whose current-or-last run deep-links into this project are
 * included; an agent with no run, or a run that links nowhere, is excluded rather than
 * fabricated onto the grid (FR-I3).
 */
export function projectAgents(
  agents: AgentActivity[] | null | undefined,
  projectId: string,
): AgentActivity[] {
  if (!agents || agents.length === 0) return [];
  const encoded = encodeProjectId(projectId);
  return agents.filter((a) => runLinksToProject(a.currentOrLastRun, encoded));
}
