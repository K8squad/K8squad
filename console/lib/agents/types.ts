// lib/agents/types.ts — read-model shapes for the Agents surface (stories 8.10 + 8.11).
//
// These mirror the apiserver's Team/Agent/Role/Run read-model (a projection of the
// `Team`/`Agent`/`Role`/`AgentRuntime`/`Run` CRDs, arch §5.1/§5.3/§8). The console is a PURE
// CONSUMER (R6 scope guard): it renders legibility only — no compose/edit (that stays 8.5) and
// no coordination affordance (claim/checkout/dispatch live server-side). Field names match the
// Go JSON tags on the read-model DTOs, not the raw CRD envelopes.

/**
 * Agent live status, DERIVED by the read-model from the Agent's current Run phase (§8 state
 * machine) + `Paused` condition reason (Epic 7.4 / story 3.7). This is the four-value legibility
 * bucket the org diagram (8.10) paints, NOT the raw seven-value `RunPhase` enum.
 */
export type AgentStatus = "idle" | "running" | "blocked" | "paused";

/** The seven-value Run state machine (arch §8) — surfaced verbatim on the Run row (8.11). */
export type RunPhase =
  | "Pending"
  | "Claiming"
  | "Running"
  | "Paused"
  | "Succeeded"
  | "Failed"
  | "Cancelled";

/** A Role badge on an Agent node (read-only, from the `Role` CRD, §5.1). */
export interface RoleBadge {
  id: string;
  name: string;
}

/**
 * An Agent node in the org diagram (8.10) and the header of the detail page (8.11). `runtimeType`
 * is the resolved `AgentRuntime.type` (§5.3, e.g. `openclaw` / `hermes` / `opencode`). `status` is
 * the derived four-value bucket; `pausedReason` (when paused) carries the §5.2 sub-state
 * (`credential` / `rate_limited`) for a legible "paused: rate-limited" chip (story 7.6/8.11).
 */
export interface OrgAgent {
  id: string;
  name: string;
  runtimeType: string;
  status: AgentStatus;
  pausedReason?: "credential" | "rate_limited" | null;
  roles: RoleBadge[];
  /** The Agent's current Run (if any) — the deep-link target for a live drill-in. */
  currentRunId?: string | null;
}

/**
 * The Team → Agent → Role org hierarchy (8.10). Rendered read-only from the `Team`/`Agent`/`Role`
 * CRDs; no new backend — a projection over existing CRDs (spec note, story 8.10).
 */
export interface TeamOrg {
  teamId: string;
  teamName: string;
  agents: OrgAgent[];
}

/** Best-effort, runtime-reported token usage (§11 OQ14) — legibility, NOT the billing authority
 * (authoritative consumption = 8.8 via the OTel metering spine). */
export interface TokenUsage {
  input?: number;
  output?: number;
  total?: number;
}

/**
 * A row in an Agent's Run history (8.11): status / duration / token usage. `traceId` deep-links to
 * the per-Run OTel trace (§17.2 / ISI-2133); `workItemRef` links back to the coordination record.
 */
export interface RunSummary {
  id: string;
  phase: RunPhase;
  pausedReason?: "credential" | "rate_limited" | null;
  workItemRef?: string | null;
  startedAt?: string | null;
  endedAt?: string | null;
  /** Server-computed elapsed seconds (or live-so-far for an active Run); best-effort. */
  durationSeconds?: number | null;
  tokens?: TokenUsage | null;
  traceId?: string | null;
}

/** The five Run-log tabs (8.11). `build` links out to the build browser (8.7); the rest are
 * `run_events` projections streamed by the shim over A2A (§7.1/§5.2). */
export type RunLogTab = "task" | "tool" | "llm" | "build" | "error";

/** One entry in a Run-log tab. `data` is tab-shaped, opaque here — the tab component renders it. */
export interface RunLogEntry {
  id: string;
  ts: string;
  tab: RunLogTab;
  summary: string;
  /** Tab-specific payload (e.g. tool name+args, LLM token counts, error trace). */
  detail?: Record<string, unknown> | null;
}
