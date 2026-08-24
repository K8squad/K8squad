// lib/dashboard.ts — story 8.8a/ISI-2906: the per-Project dashboard payload types
// + the browser-side fetcher for the BFF route.
//
// Every tile carries its own availability envelope (8.8a per-tile degradation):
// an unavailable source renders an explicit "not configured" state — NEVER a
// fake number (FR-I3 provenance). Types mirror internal/apiserver/dashboard.go
// exactly; the Go structs are the contract owner.

export type TileStatus = {
  available: boolean;
  reason?: string;
};

export type TicketSummary = {
  id: string;
  title: string;
  status: string;
  updatedAt?: string;
};

export type ThroughputPoint = { date: string; count: number };

export type PendingApproval = {
  ticketId: string;
  title: string;
  requestingAgent?: string;
  runId?: string;
  raisedAt?: string;
};

export type TicketsTile = TileStatus & {
  byStatus?: Record<string, number>;
  recent?: TicketSummary[];
  throughput?: ThroughputPoint[];
  pendingApprovals?: PendingApproval[];
  canAct?: boolean;
};

export type PullRequest = {
  number: number;
  title: string;
  branch?: string;
  headSha?: string;
  runName?: string;
  reviewState?: string;
  updatedAt?: string;
};

export type PRTile = TileStatus & {
  readyForReview?: PullRequest[];
  draft?: PullRequest[];
  blocked?: PullRequest[];
  merged?: PullRequest[];
};

export type TokenTrendPoint = { date: string; tokens: number };

export type ConsumptionTile = TileStatus & {
  totalTokens?: number;
  estimatedCost?: number;
  currency?: string;
  trend?: TokenTrendPoint[];
};

export type LiveRun = {
  name: string;
  workItem?: string;
  agent?: string;
  phase: string;
  claimedAt?: string;
  pausedReason?: string;
  resumeAt?: string;
  fallbackModel?: string;
};

export type LiveRunsTile = TileStatus & { runs: LiveRun[] };

export type ProjectDashboard = {
  project: { name: string; namespace: string };
  tickets: TicketsTile;
  pullRequests: PRTile;
  consumption: ConsumptionTile;
  liveRuns: LiveRunsTile;
};

/**
 * Fetch the 8.8a dashboard payload for a Project through the BFF choke point.
 * Non-2xx raises with the status so callers can render an error surface —
 * the BFF relays the apiserver's status verbatim (404 = foreign/unknown
 * Project, existence-hiding).
 */
export async function fetchProjectDashboard(
  projectId: string,
): Promise<ProjectDashboard> {
  const res = await fetch(
    `/api/projects/${encodeURIComponent(projectId)}/dashboard`,
    { cache: "no-store" },
  );
  if (!res.ok) {
    throw new Error(`dashboard fetch failed: ${res.status}`);
  }
  return (await res.json()) as ProjectDashboard;
}

/** Sum of a status map, used by the KPI card total. */
export function totalTickets(byStatus?: Record<string, number>): number {
  if (!byStatus) return 0;
  return Object.values(byStatus).reduce((a, b) => a + b, 0);
}

/** Format a token count for the consumption KPI card (legibility, OQ14). */
export function formatTokens(n: number): string {
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`;
  if (n >= 1_000) return `${(n / 1_000).toFixed(1)}k`;
  return String(n);
}
