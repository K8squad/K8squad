// lib/agents/api.ts — BFF client for the Agents surface (stories 8.10 + 8.11).
//
// Same posture as the discussion client (lib/discussion/api.ts): a pure consumer behind the ONE
// deny-by-default BFF authorization choke point (§13 / ADR-013). Deny is existence-hiding — 404
// NOT 403 (the 8.7d pattern): a Team-B principal reading a Team-A Agent/Run sees "missing", never
// another Team's structure. This module holds NO authorization decision of its own and issues NO
// mutating verb (R6 scope guard: the org diagram + agent detail are read/legibility surfaces).

import type { RunLogTab, RunSummary, TeamOrg, OrgAgent } from "./types";

export type AgentsOutcome = "ok" | "not-found" | "error";

/** Collapse an HTTP status into an outcome; 401/403/404 → not-found (no foreign existence leak). */
export function classifyStatus(status: number): AgentsOutcome {
  if (status >= 200 && status < 300) return "ok";
  if (status === 401 || status === 403 || status === 404) return "not-found";
  return "error";
}

export class AgentsApiError extends Error {
  constructor(
    public readonly status: number,
    public readonly outcome: AgentsOutcome,
  ) {
    super(`agents api: status ${status} (${outcome})`);
    this.name = "AgentsApiError";
  }
}

/** Minimal fetch surface so the client is trivially testable with a stub. */
export type FetchLike = (
  input: string,
  init?: { method?: string; headers?: Record<string, string> },
) => Promise<{ ok: boolean; status: number; json: () => Promise<unknown> }>;

export interface AgentsClient {
  getTeamOrg(teamId: string): Promise<TeamOrg>;
  getAgent(agentId: string): Promise<OrgAgent>;
  listAgentRuns(
    agentId: string,
    opts?: { limit?: number; offset?: number },
  ): Promise<RunSummary[]>;
  getRunLogs(runId: string, tab: RunLogTab): Promise<unknown[]>;
}

const enc = encodeURIComponent;

async function readJson<T>(res: {
  ok: boolean;
  status: number;
  json: () => Promise<unknown>;
}): Promise<T> {
  if (!res.ok) throw new AgentsApiError(res.status, classifyStatus(res.status));
  return (await res.json()) as T;
}

/** Construct a BFF-backed Agents client. `fetchImpl` defaults to global fetch. */
export function createAgentsClient(
  fetchImpl: FetchLike = fetch as unknown as FetchLike,
): AgentsClient {
  return {
    async getTeamOrg(teamId) {
      const res = await fetchImpl(`/api/teams/${enc(teamId)}/org`, {
        method: "GET",
      });
      return readJson<TeamOrg>(res);
    },

    async getAgent(agentId) {
      const res = await fetchImpl(`/api/agents/${enc(agentId)}`, {
        method: "GET",
      });
      return readJson<OrgAgent>(res);
    },

    async listAgentRuns(agentId, opts) {
      const p = new URLSearchParams();
      if (opts?.limit != null) p.set("limit", String(opts.limit));
      if (opts?.offset != null) p.set("offset", String(opts.offset));
      const qs = p.toString();
      const res = await fetchImpl(
        `/api/agents/${enc(agentId)}/runs${qs ? `?${qs}` : ""}`,
        { method: "GET" },
      );
      return readJson<RunSummary[]>(res);
    },

    async getRunLogs(runId, tab) {
      const res = await fetchImpl(`/api/runs/${enc(runId)}/logs/${enc(tab)}`, {
        method: "GET",
      });
      return readJson<unknown[]>(res);
    },
  };
}
