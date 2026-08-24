"use client";

// components/agents/TeamOrgDiagram.tsx — the live squad org chart (story 8.10).
//
// Renders the Team → Agent → Role hierarchy from the apiserver read-model (projected from the
// `Team`/`Agent`/`Role` CRDs, §5.1) — READ-ONLY legibility, NOT a compose/edit view (that stays
// 8.5) and NOT a coordination path (R6 scope guard: no claim/checkout/dispatch affordance rides
// here). Each Agent node shows real-time status, its runtime type (`AgentRuntime`, §5.3), and role
// badges; status updates LIVE over SSE (useTeamStatus, same one-bus discipline as 8.2); clicking a
// node deep-links to the agent detail page (8.11).

import { useEffect, useState } from "react";
import Link from "next/link";
import type { TeamOrg, OrgAgent } from "@/lib/agents/types";
import { createAgentsClient, AgentsApiError } from "@/lib/agents/api";
import { useTeamStatus } from "@/lib/agents/useTeamStatus";
import { StatusChip } from "./StatusChip";

type LoadState =
  | { kind: "loading" }
  | { kind: "not-found" }
  | { kind: "error" }
  | { kind: "ok"; org: TeamOrg };

/**
 * Overlay a live SSE status delta onto the server-rendered snapshot so a phase change repaints a
 * single node without a refetch (8.10 "status updates live over SSE").
 */
function withLiveStatus(
  agent: OrgAgent,
  delta: ReturnType<typeof useTeamStatus>["deltas"][string] | undefined,
): OrgAgent {
  if (!delta) return agent;
  return {
    ...agent,
    status: delta.status,
    pausedReason: delta.pausedReason ?? null,
    currentRunId: delta.currentRunId ?? agent.currentRunId ?? null,
  };
}

export function TeamOrgDiagram({ teamId }: { teamId: string }) {
  const [state, setState] = useState<LoadState>({ kind: "loading" });
  const { deltas, status: streamStatus } = useTeamStatus(teamId);

  useEffect(() => {
    let cancelled = false;
    const client = createAgentsClient();
    client
      .getTeamOrg(teamId)
      .then((org) => {
        if (!cancelled) setState({ kind: "ok", org });
      })
      .catch((err: unknown) => {
        if (cancelled) return;
        if (err instanceof AgentsApiError && err.outcome === "not-found") {
          setState({ kind: "not-found" });
        } else {
          setState({ kind: "error" });
        }
      });
    return () => {
      cancelled = true;
    };
  }, [teamId]);

  if (state.kind === "loading") {
    return <p className="muted">Loading org diagram…</p>;
  }
  // Deny is existence-hiding: a foreign / missing Team renders identically (404-not-403).
  if (state.kind === "not-found") {
    return <p className="muted">This team org diagram is not available.</p>;
  }
  if (state.kind === "error") {
    return <p className="muted">Couldn’t load the org diagram. Try again.</p>;
  }

  const { org } = state;

  return (
    <section className="org-diagram" aria-label={`Org diagram for ${org.teamName}`}>
      <header className="org-diagram__head">
        <h2 className="org-diagram__team">{org.teamName}</h2>
        <span className={`stream-status stream-status--${streamStatus}`}>
          {streamStatus === "open" ? "live" : streamStatus}
        </span>
      </header>

      {org.agents.length === 0 ? (
        <p className="muted">No agents composed on this team yet.</p>
      ) : (
        <ul className="org-diagram__agents">
          {org.agents.map((a) => {
            const agent = withLiveStatus(a, deltas[a.id]);
            return (
              <li key={agent.id} className="org-node">
                {/* Clicking an Agent node deep-links to its detail page (8.11). */}
                <Link
                  href={`/agents/${encodeURIComponent(agent.id)}`}
                  className="org-node__link"
                >
                  <div className="org-node__head">
                    <span className="org-node__name">{agent.name}</span>
                    <StatusChip
                      status={agent.status}
                      pausedReason={agent.pausedReason}
                    />
                  </div>
                  <div className="org-node__meta">
                    <span
                      className="org-node__runtime"
                      title={`runtime: ${agent.runtimeType}`}
                    >
                      {agent.runtimeType}
                    </span>
                    <span className="org-node__roles">
                      {agent.roles.map((r) => (
                        <span key={r.id} className="role-badge">
                          {r.name}
                        </span>
                      ))}
                    </span>
                  </div>
                </Link>
              </li>
            );
          })}
        </ul>
      )}
    </section>
  );
}
