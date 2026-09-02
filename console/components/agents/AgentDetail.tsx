"use client";

// components/agents/AgentDetail.tsx — agent detail page with Run history + logs (story 8.11).
//
// Reached from the org diagram (8.10) or overview (8.1). Shows the Agent header (name, runtime,
// role badges, live status) and a Run list — status / duration / token usage — from the apiserver
// read-model (projected from the `Run` CRDs, §8; no new backend). Drilling into a Run expands
// tabbed logs (RunLogs, 8.11) with a live SSE tail for an active Run and a deep-link to its OTel
// trace. READ-ONLY (R6): token counts are runtime-reported / best-effort (§11 OQ14) — legibility,
// NOT the billing authority (authoritative consumption = 8.8). No mutate/claim/kill affordance.

import { useEffect, useState } from "react";
import type { OrgAgent, RunSummary } from "@/lib/agents/types";
import { createAgentsClient, AgentsApiError } from "@/lib/agents/api";
import { StatusChip } from "./StatusChip";
import { RunLogs } from "./RunLogs";
import { ToolUsagePanel } from "./ToolUsagePanel";

type LoadState =
  | { kind: "loading" }
  | { kind: "not-found" }
  | { kind: "error" }
  | { kind: "ok"; agent: OrgAgent; runs: RunSummary[] };

function fmtDuration(s?: number | null): string {
  if (s == null) return "—";
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  const rem = s % 60;
  if (m < 60) return `${m}m ${rem}s`;
  const h = Math.floor(m / 60);
  return `${h}h ${m % 60}m`;
}

function fmtTokens(r: RunSummary): string {
  const t = r.tokens;
  if (!t) return "—";
  if (t.total != null) return `${t.total.toLocaleString()}`;
  const parts: string[] = [];
  if (t.input != null) parts.push(`${t.input.toLocaleString()} in`);
  if (t.output != null) parts.push(`${t.output.toLocaleString()} out`);
  return parts.length ? parts.join(" / ") : "—";
}

export function AgentDetail({ agentId }: { agentId: string }) {
  const [state, setState] = useState<LoadState>({ kind: "loading" });
  const [openRun, setOpenRun] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    const client = createAgentsClient();
    Promise.all([client.getAgent(agentId), client.listAgentRuns(agentId)])
      .then(([agent, runs]) => {
        if (!cancelled) setState({ kind: "ok", agent, runs });
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
  }, [agentId]);

  if (state.kind === "loading") return <p className="muted">Loading agent…</p>;
  // Deny is existence-hiding: a foreign / missing Agent renders identically (404-not-403).
  if (state.kind === "not-found")
    return <p className="muted">This agent is not available.</p>;
  if (state.kind === "error")
    return <p className="muted">Couldn’t load this agent. Try again.</p>;

  const { agent, runs } = state;

  return (
    <section className="agent-detail" aria-label={`Detail for ${agent.name}`}>
      <header className="agent-detail__head card">
        <div className="agent-detail__id">
          <h1 className="agent-detail__name">{agent.name}</h1>
          <StatusChip status={agent.status} pausedReason={agent.pausedReason} />
        </div>
        <div className="agent-detail__meta">
          <span className="org-node__runtime" title={`runtime: ${agent.runtimeType}`}>
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
      </header>

      <div className="card">
        <h2 className="agent-detail__runs-title">Run history</h2>
        <p className="muted agent-detail__tokens-note">
          Token counts are runtime-reported / best-effort — legibility,
          not the billing authority (authoritative consumption is the dashboard).
        </p>
        {runs.length === 0 ? (
          <p className="muted">No runs yet for this agent.</p>
        ) : (
          <table className="run-history">
            <thead>
              <tr>
                <th scope="col">Run</th>
                <th scope="col">Status</th>
                <th scope="col">Duration</th>
                <th scope="col">Tokens</th>
                <th scope="col">Work item</th>
                <th scope="col" aria-label="drill in" />
              </tr>
            </thead>
            <tbody>
              {runs.map((r) => {
                const isOpen = openRun === r.id;
                return [
                  <tr key={r.id} className="run-history__row">
                    <td>
                      <code>{r.id}</code>
                    </td>
                    <td>
                      <span
                        className={`run-phase run-phase--${r.phase.toLowerCase()}`}
                      >
                        {r.phase}
                        {r.pausedReason ? ` (${r.pausedReason})` : ""}
                      </span>
                    </td>
                    <td>{fmtDuration(r.durationSeconds)}</td>
                    <td>{fmtTokens(r)}</td>
                    <td>
                      {r.workItemRef ? <code>{r.workItemRef}</code> : "—"}
                    </td>
                    <td>
                      <button
                        type="button"
                        className="run-history__drill"
                        aria-expanded={isOpen}
                        onClick={() => setOpenRun(isOpen ? null : r.id)}
                      >
                        {isOpen ? "Hide logs" : "View logs"}
                      </button>
                    </td>
                  </tr>,
                  isOpen ? (
                    <tr key={`${r.id}-logs`} className="run-history__logs-row">
                      <td colSpan={6}>
                        <RunLogs
                          runId={r.id}
                          phase={r.phase}
                          traceId={r.traceId}
                        />
                      </td>
                    </tr>
                  ) : null,
                ];
              })}
            </tbody>
          </table>
        )}
      </div>

      {/* Epic D (ISI-3288, plan §2.4 story D3): per-agent tool-usage panel —
          the D2 ksquad_* metrics aggregate. Degraded states render honestly. */}
      <ToolUsagePanel agentId={agent.name} />
    </section>
  );
}
