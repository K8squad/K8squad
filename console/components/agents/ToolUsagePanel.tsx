"use client";

// components/agents/ToolUsagePanel.tsx — per-agent tool-usage panel (Epic D / ISI-3288, D3).
//
// The D2 metrics (ksquad_tool_calls_total, ksquad_skill_loads_total,
// ksquad_mcp_call_duration_seconds) aggregated per agent by the apiserver read
// model. READ-ONLY (R6): counts are observational best-effort telemetry, not a
// billing authority. Unavailable (501/503) renders an explicit degraded state —
// never a fabricated table (FR-I3 provenance). Per-RUN usage lives in the Run's
// OTel trace (spans carry ksquad.run.id) — the Run rows deep-link to it.

import { useEffect, useState } from "react";
import {
  fetchToolUsage,
  totalToolCalls,
  totalSkillLoads,
  formatSeconds,
  type ToolUsageAgent,
  type MCPStat,
} from "@/lib/toolusage";

type LoadState =
  | { kind: "loading" }
  | { kind: "unavailable"; reason: string }
  | { kind: "ok"; agent: ToolUsageAgent | null; mcp: MCPStat[] };

export function ToolUsagePanel({ agentId }: { agentId: string }) {
  const [state, setState] = useState<LoadState>({ kind: "loading" });

  useEffect(() => {
    let cancelled = false;
    fetchToolUsage(agentId)
      .then((payload) => {
        if (cancelled) return;
        const agent = payload.agents.find((a) => a.agent === agentId) ?? null;
        setState({ kind: "ok", agent, mcp: payload.mcp });
      })
      .catch(() => {
        if (!cancelled) {
          // 501 = read model not wired; 503 = operator metrics unreachable —
          // both render as the same honest "not available" state.
          setState({ kind: "unavailable", reason: "tool-usage read model" });
        }
      });
    return () => {
      cancelled = true;
    };
  }, [agentId]);

  if (state.kind === "loading")
    return (
      <section className="card tool-usage" aria-label="Tool usage">
        <h2>Tool usage</h2>
        <p className="muted">Loading tool usage…</p>
      </section>
    );

  if (state.kind === "unavailable")
    return (
      <section className="card tool-usage" aria-label="Tool usage">
        <h2>Tool usage</h2>
        <p className="muted">
          Tool usage is not available ({state.reason} not reachable).
        </p>
      </section>
    );

  const { agent, mcp } = state;

  if (!agent || (agent.toolCalls.length === 0 && agent.skillLoads.length === 0))
    return (
      <section className="card tool-usage" aria-label="Tool usage">
        <h2>Tool usage</h2>
        <p className="muted">
          No tool activity recorded for this agent yet.
        </p>
      </section>
    );

  return (
    <section className="card tool-usage" aria-label="Tool usage">
      <h2>Tool usage</h2>
      <p className="muted tool-usage__note">
        Aggregate of this agent&apos;s tool calls and skill loads (Epic D
        telemetry). Per-run detail lives in each Run&apos;s OTel trace.
      </p>

      <div className="tool-usage__kpis">
        <div className="tool-usage__kpi">
          <span className="tool-usage__kpi-value">
            {totalToolCalls(agent).toLocaleString()}
          </span>
          <span className="tool-usage__kpi-label">tool calls</span>
        </div>
        <div className="tool-usage__kpi">
          <span className="tool-usage__kpi-value">
            {totalSkillLoads(agent).toLocaleString()}
          </span>
          <span className="tool-usage__kpi-label">skill loads</span>
        </div>
      </div>

      {agent.toolCalls.length > 0 && (
        <table className="tool-usage__table">
          <caption className="sr-only">Tool calls by tool</caption>
          <thead>
            <tr>
              <th scope="col">Tool</th>
              <th scope="col">Skill</th>
              <th scope="col" className="num">Calls</th>
            </tr>
          </thead>
          <tbody>
            {agent.toolCalls.map((t) => (
              <tr key={`${t.tool}/${t.skill ?? ""}`}>
                <td>{t.tool}</td>
                <td>{t.skill ?? "—"}</td>
                <td className="num">{t.calls.toLocaleString()}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      {agent.skillLoads.length > 0 && (
        <table className="tool-usage__table">
          <caption className="sr-only">Skill loads</caption>
          <thead>
            <tr>
              <th scope="col">Skill</th>
              <th scope="col" className="num">Loads</th>
            </tr>
          </thead>
          <tbody>
            {agent.skillLoads.map((s) => (
              <tr key={s.skill}>
                <td>{s.skill}</td>
                <td className="num">{s.loads.toLocaleString()}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      {mcp.length > 0 && (
        <>
          <h3 className="tool-usage__mcp-title">MCP servers (platform-wide)</h3>
          <table className="tool-usage__table">
            <caption className="sr-only">
              MCP-served tool calls and mean duration
            </caption>
            <thead>
              <tr>
                <th scope="col">Server</th>
                <th scope="col">Tool</th>
                <th scope="col" className="num">Calls</th>
                <th scope="col" className="num">Avg</th>
              </tr>
            </thead>
            <tbody>
              {mcp.map((m) => (
                <tr key={`${m.server}/${m.tool}`}>
                  <td>{m.server}</td>
                  <td>{m.tool}</td>
                  <td className="num">{m.calls.toLocaleString()}</td>
                  <td className="num">{formatSeconds(m.avgSeconds)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </>
      )}
    </section>
  );
}
