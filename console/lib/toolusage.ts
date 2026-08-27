// lib/toolusage.ts — Epic D / ISI-3288 (plan §2.4 story D3): the per-agent
// tool-usage panel's data contract.
//
// Mirrors internal/apiserver/toolusage.go exactly (the Go read model is the
// contract owner): GET /api/telemetry/tool-usage answers { agents, mcp } —
// the per-agent aggregate of the operator's ksquad_* tool metrics (D2), plus
// the platform-scoped MCP table (duration histograms are {server,tool}-
// labeled only — per-agent attribution would explode cardinality, 13.6).
//
// Per-RUN tool usage is the Run's OTel trace (spans carry ksquad.run.id);
// this panel answers the per-AGENT question. Read-only, best-effort: an
// unavailable read model renders an explicit degraded state — never a fake
// number (FR-I3 provenance).

export type ToolCallStat = {
  tool: string;
  skill?: string;
  calls: number;
};

export type SkillLoadStat = {
  skill: string;
  loads: number;
};

export type MCPStat = {
  server: string;
  tool: string;
  calls: number;
  avgSeconds: number;
};

export type ToolUsageAgent = {
  agent: string;
  toolCalls: ToolCallStat[];
  skillLoads: SkillLoadStat[];
  mcp: MCPStat[];
};

export type ToolUsagePayload = {
  agents: ToolUsageAgent[];
  mcp: MCPStat[];
};

/**
 * Fetch the tool-usage aggregate through the BFF choke point. `agent` scopes
 * the request to one agent (the apiserver still answers the full MCP table).
 * Non-2xx raises with the status so the caller renders the degraded state.
 */
export async function fetchToolUsage(
  agent?: string,
): Promise<ToolUsagePayload> {
  const qs = agent ? `?agent=${encodeURIComponent(agent)}` : "";
  const res = await fetch(`/api/telemetry/tool-usage${qs}`, {
    cache: "no-store",
  });
  if (!res.ok) {
    throw new Error(`tool-usage fetch failed: ${res.status}`);
  }
  return (await res.json()) as ToolUsagePayload;
}

/** Total tool calls across an agent's rows — the panel's headline number. */
export function totalToolCalls(a: ToolUsageAgent): number {
  return a.toolCalls.reduce((n, t) => n + t.calls, 0);
}

/** Total skill loads across an agent's rows. */
export function totalSkillLoads(a: ToolUsageAgent): number {
  return a.skillLoads.reduce((n, s) => n + s.loads, 0);
}

/** Format a seconds value for the MCP table (legibility, OQ14). */
export function formatSeconds(s: number): string {
  if (s < 1) return `${Math.round(s * 1000)}ms`;
  return `${s.toFixed(1)}s`;
}
