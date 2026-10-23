// lib/overview/agentActivity.ts — the FE contract + pure projections behind the
// Paperclip-style AgentRunCard (ISI-5707 WS-2, parent plan ISI-5697 rev2,
// approved design ISI-5699 mock B).
//
// These types mirror the WS-1a agent read model (ISI-5704): the enriched
// `GET /api/squad/agents` response gains, per agent, its current-or-last run with
// a SERVER-classified last-thinking snippet (via lib/run-activity, never the raw
// [run]/[tool]/[result] stream — lesson from ISI-5695/5696) plus the work-item /
// discussion deep-links the card chips to. Kept here (not inlined in the card) so
// the WS-3 fleet recompose and WS-4 project overview fold the same shape and the
// same honest projections. Keep this in lockstep with WS-1a's wire shape.
//
// Fabrication discipline (FR-I3, ISI-4229 degrade-don't-blank): an unavailable
// source renders `—`, only an authoritative zero renders `0`, and a run with
// nothing reported yet renders `waiting for first step…` — never a blank or an
// invented line.

/** The aggregate status WS-1a classifies each agent into (counts{running,idle,blocked}). */
export type AgentState = "running" | "idle" | "blocked";

/** What the card actually renders — the aggregate state plus the two FE-only
 * presentation modes that are not agent states: no run on record, and in-flight. */
export type AgentCardState = AgentState | "empty" | "loading";

/** One agent's current (running) or most-recent (idle/blocked) run, as projected by
 * WS-1a. Every field past the ids is nullable on the wire (Go nil → null / absent):
 * a freshly-minted run carries no snippet yet, an agent-less run no work item. */
export interface AgentCurrentOrLastRun {
  runId: string;
  phase: string;
  /** Ticket ref for the deep-link chip label (e.g. "ISI-5680"). */
  workItemRef?: string | null;
  /** In-console deep-link to the ticket the agent worked on. */
  workItemUrl?: string | null;
  /** In-console deep-link to the discussion the agent worked on (when there is no ticket). */
  discussionUrl?: string | null;
  /** The latest CLASSIFIED thinking snippet (server-side via run-activity), NOT the raw stream. */
  lastThinkingSnippet?: string | null;
  /** ISO-8601 instant of the agent's last activity, for the relative-time line. */
  lastActivityAt?: string | null;
}

/** One agent row from the WS-1a enriched `GET /api/squad/agents` read model. */
export interface AgentActivity {
  /** Stable identity used both as the React key and the deterministic avatar seed. */
  agentId: string;
  /** Human name — never the `intake-<uuid>` run id (the whole point of the redesign). */
  name: string;
  /** Role sub-label (e.g. "Backend Engineer"). */
  role?: string | null;
  state: AgentState;
  currentOrLastRun?: AgentCurrentOrLastRun | null;
}

/**
 * Resolve the card's render state. `loading` wins (the read is in flight); then an
 * agent with no run on record is `empty` (never ran — FR-I3 "No run yet"); otherwise
 * the WS-1a aggregate state drives the chip and the thinking line.
 */
export function resolveCardState(
  agent: AgentActivity | null | undefined,
  loading = false,
): AgentCardState {
  if (loading || !agent) return "loading";
  if (!agent.currentOrLastRun) return "empty";
  return agent.state;
}

/** Up-to-two-letter initials from a display name (`Claude Code` → `CC`, `Winston` → `WI`). */
export function agentInitials(name: string): string {
  const parts = (name ?? "")
    .trim()
    .split(/[\s._/-]+/)
    .filter(Boolean);
  if (parts.length === 0) return "?";
  if (parts.length === 1) return parts[0].slice(0, 2).toUpperCase();
  return (parts[0][0] + parts[parts.length - 1][0]).toUpperCase();
}

/**
 * Deterministic avatar hue from the agent's stable seed (id, falling back to name).
 * Returned as a self-contained HSL so the swatch reads identically in light and dark
 * without minting a new theme token: fixed saturation/lightness keep white initials
 * legible on every hue. Same seed → same colour, every render (FNV-1a over the seed).
 */
export function avatarHue(seed: string): number {
  let h = 0x811c9dc5;
  for (let i = 0; i < (seed ?? "").length; i++) {
    h ^= seed.charCodeAt(i);
    h = Math.imul(h, 0x01000193);
  }
  return (h >>> 0) % 360;
}

export function avatarColor(seed: string): string {
  return `hsl(${avatarHue(seed)} 55% 52%)`;
}

/** One resolved deep-link chip: a href + a short label + whether it is the discussion. */
export interface AgentDeepLink {
  href: string;
  label: string;
  kind: "ticket" | "discussion";
}

/**
 * The single work-item / discussion chip for the card. A ticket wins over a
 * discussion (the agent's primary unit of work); the discussion is the fallback
 * for a party/discussion-only agent. Returns null when the run deep-links nowhere,
 * so the card renders no dead chip (degrade-don't-blank) rather than an invented one.
 */
export function resolveDeepLink(
  run: AgentCurrentOrLastRun | null | undefined,
): AgentDeepLink | null {
  if (!run) return null;
  if (run.workItemUrl) {
    return {
      href: run.workItemUrl,
      label: run.workItemRef || "work item",
      kind: "ticket",
    };
  }
  if (run.discussionUrl) {
    return { href: run.discussionUrl, label: "discussion", kind: "discussion" };
  }
  return null;
}

/**
 * The classified last-thinking line, with FR-I3 fabrication discipline applied:
 *   - a present snippet renders verbatim (already server-classified);
 *   - a `running` run with nothing reported yet renders `waiting for first step…`;
 *   - anything else with no snippet renders `—` (unavailable), never a blank or a guess.
 */
export const WAITING_SNIPPET = "waiting for first step…";
export const UNAVAILABLE = "—";

export function thinkingLine(
  state: AgentCardState,
  run: AgentCurrentOrLastRun | null | undefined,
): string {
  const snippet = run?.lastThinkingSnippet?.trim();
  if (snippet) return snippet;
  if (state === "running") return WAITING_SNIPPET;
  return UNAVAILABLE;
}

/**
 * Compact relative time for the last-activity line (`now`, `2m ago`, `22m ago`, `3h ago`).
 * `now` is injected (ms since epoch) so the projection stays pure/testable and SSR-stable.
 * An absent/invalid instant renders `—` (unavailable), never a fabricated time.
 */
export function relativeActivityShort(
  iso: string | null | undefined,
  now: number,
): string {
  if (!iso) return UNAVAILABLE;
  const t = new Date(iso).getTime();
  if (!Number.isFinite(t)) return UNAVAILABLE;
  const diff = Math.floor((now - t) / 1000);
  if (diff < 60) return "now";
  if (diff < 3600) return `${Math.floor(diff / 60)}m ago`;
  if (diff < 86400) return `${Math.floor(diff / 3600)}h ago`;
  return `${Math.floor(diff / 86400)}d ago`;
}

// ---------------------------------------------------------------------------
// Fetch client (thin — the BFF proxies the §13 choke point). Added by the WS-3
// fleet recompose (ISI-5708); kept here so WS-4's project overview folds the
// same shape. Mirrors the apiserver AgentActivityList (agentactivity.go).
// ---------------------------------------------------------------------------

/** The fleet/Team rollup of agent states — running+idle+blocked == number of agents. */
export interface ActivityCounts {
  running: number;
  idle: number;
  blocked: number;
}

/** The GET /api/squad/agents/activity payload (WS-1a). */
export interface AgentActivityList {
  agents: AgentActivity[];
  counts: ActivityCounts;
  /** Set only for a global-admin caller (every squad's agents). */
  fleet?: boolean;
}

/** The read-model resolves to one of these; `not-available` is the honest degrade
 * (endpoint unrolled / caller can't see a Team) — NOT an error to surface loudly. */
export type AgentActivityState =
  | { kind: "loading" }
  | { kind: "not-available"; status: number }
  | { kind: "error"; status: number }
  | { kind: "ready"; data: AgentActivityList };

const ZERO_COUNTS: ActivityCounts = { running: 0, idle: 0, blocked: 0 };

/** 404 (no team / endpoint not mounted) and 501 (read model unwired) degrade; the
 * rest is a retryable error — mirrors classifySeriesStatus (projectSeries.ts). */
export function classifyActivityStatus(
  status: number,
): Extract<AgentActivityState, { kind: "not-available" | "error" }> {
  return status === 404 || status === 501
    ? { kind: "not-available", status }
    : { kind: "error", status };
}

/** Fetch the agent-activity aggregate from the BFF. Degrades honestly — never throws
 * the page down (degrade-don't-blank, ISI-4229). */
export async function fetchAgentActivity(
  signal?: AbortSignal,
): Promise<AgentActivityState> {
  try {
    const res = await fetch("/api/squad/agents/activity", {
      headers: { accept: "application/json" },
      signal,
    });
    if (!res.ok) return classifyActivityStatus(res.status);
    const body = (await res.json()) as Partial<AgentActivityList>;
    return {
      kind: "ready",
      data: {
        agents: body.agents ?? [],
        counts: body.counts ?? ZERO_COUNTS,
        fleet: body.fleet,
      },
    };
  } catch {
    // AbortError / network failure — retryable, not a degrade seam.
    return { kind: "error", status: 0 };
  }
}
