// lib/credentials.ts — shared types + pure derivations for the Credentials screen (story 8.6).
//
// The screen's data contract mirrors the apiserver's GET /api/credentials projection
// (internal/apiserver/credentials.go). All presentation derivations live here as PURE functions so
// the component stays a thin render and the derivations are unit-testable (Vitest) without a DOM.
//
// Honesty rules carried from the spec (8.6 / 8.8b no-placeholder discipline / FR-I3):
//   - an expiry the read model cannot know renders "—" (unknown), NEVER a fabricated horizon;
//   - health is the closed set connected | refreshing | expired | unknown — everything else maps
//     to unknown rather than guessing.

/** One row of the credentials table (mirror of the apiserver's AgentCredentialRow). */
export interface AgentCredentialRow {
  agent: string;
  namespace: string;
  runtime: string;
  model?: string;
  /** "namespace/name" of the per-user Secret (FR-G1 — never a master credential). */
  credentialRef: string;
  credentialClass?: string;
  expiresAt?: string;
  expiresKnown: boolean;
  health: string;
  pausedRuns?: PausedRunRef[];
}

/** One Run paused on a credential hold (deep-links to the run stream 8.2 / detail 8.11). */
export interface PausedRunRef {
  name: string;
  reason: string;
  since?: string;
}

/** GET /api/credentials payload (mirror of the apiserver's CredentialsOverview). */
export interface CredentialsOverview {
  team: string;
  agents: AgentCredentialRow[];
  connectClaude: boolean;
}

/** Classified fetch outcome for the screen — 401/403/404 collapse (existence-hiding, AC4-style). */
export type CredentialsOutcome =
  | { kind: "ok"; data: CredentialsOverview }
  | { kind: "not-found" } // unauthenticated / denied / no team — indistinguishable by design
  | { kind: "unconfigured" } // apiserver's documented 501 (read model not wired yet)
  | { kind: "error" }; // 5xx / network — the apiserver itself is unhappy

/** Token-type label for the row: the credential-class annotation when present, else the shape of
 *  the Secret ref is unknown to the console — render honestly rather than guess OAuth-vs-API-key. */
export function tokenTypeLabel(row: AgentCredentialRow): string {
  switch (row.credentialClass) {
    case "claude_oauth":
      return "OAuth";
    case "api_key":
      return "API key";
    case "mistral_api_key":
      return "API key · Mistral";
    case "byo_endpoint":
      return "BYO endpoint";
    case "human-seat":
      return "OAuth · seat";
    case "service-account":
      return "Service account";
    default:
      return "—";
  }
}

/** Expiry cell text: known horizon formatted compactly; unknown renders the em dash (never a
 *  fabricated number). Static keys ("API key") surface as "— (static)" only when the class says so. */
export function expiryLabel(row: AgentCredentialRow, now: Date = new Date()): string {
  if (
    row.credentialClass === "api_key" ||
    row.credentialClass === "byo_endpoint" ||
    row.credentialClass === "mistral_api_key"
  ) {
    return "— (static)";
  }
  if (!row.expiresKnown || !row.expiresAt) {
    return "—";
  }
  const at = new Date(row.expiresAt);
  const diffMs = at.getTime() - now.getTime();
  if (diffMs <= 0) {
    return `expired ${formatAgo(-diffMs, now, at)} ago`;
  }
  return `in ${formatDuration(diffMs)}`;
}

/** True when a known horizon is within the soon-window (soonThresholdMs) — drives the
 *  "Expiring soon" badge. Unknown horizons are never "soon" (honesty over alarm). */
export function expiringSoon(row: AgentCredentialRow, now: Date = new Date(), soonThresholdMs = 45 * 60 * 1000): boolean {
  if (!row.expiresKnown || !row.expiresAt) return false;
  const diff = new Date(row.expiresAt).getTime() - now.getTime();
  return diff > 0 && diff <= soonThresholdMs;
}

/** Health badge state for a row: the closed set + the paused/soon overlays the mock 05 shows. */
export type HealthBadge = {
  label: string;
  tone: "ok" | "warn" | "bad" | "idle";
};

export function healthBadge(row: AgentCredentialRow, now: Date = new Date()): HealthBadge {
  if (row.health === "expired" || row.pausedRuns?.length) {
    return { label: "Expired · paused", tone: "bad" };
  }
  if (row.health === "refreshing") {
    return { label: "Refreshing", tone: "warn" };
  }
  if (row.health === "connected") {
    if (expiringSoon(row, now)) {
      return { label: "Expiring soon", tone: "warn" };
    }
    return { label: "Valid", tone: "ok" };
  }
  return { label: "Unknown", tone: "idle" };
}

/** The paused-on-expiry banner selection (8.6 AC / S10): the most recent credential hold across
 *  the rows — the screen's clearest operator signal. Null when nothing is held. Timestamps are
 *  compared as parsed instants, never as strings: Go's encoding/json emits RFC3339Nano, where a
 *  sub-second suffix would sort before "Z" lexicographically and pick the WRONG run (PR #87
 *  review). NaN comparisons are false, preserving first-wins when a `since` is absent. */
export interface BannerHold {
  agent: string;
  run: PausedRunRef;
}

const holdTs = (r: PausedRunRef): number => (r.since ? Date.parse(r.since) : Number.NaN);

export function bannerHold(rows: AgentCredentialRow[]): BannerHold | null {
  let worst: BannerHold | null = null;
  for (const row of rows) {
    for (const run of row.pausedRuns ?? []) {
      if (worst === null || holdTs(run) > holdTs(worst.run)) {
        worst = { agent: row.agent, run };
      }
    }
  }
  return worst;
}

/** Relative "x ago" for a past instant (banner copy mirrors mock 05's "expired 6m ago"). */
function formatAgo(diffMs: number, _now: Date, at: Date): string {
  void _now;
  void at;
  return formatDuration(diffMs);
}

/** Compact human duration: 41m / 5h 12m / 2h 48m / 9d — mock 05's expiry idiom. */
export function formatDuration(ms: number): string {
  const m = Math.round(ms / 60000);
  if (m < 1) return "<1m";
  if (m < 60) return `${m}m`;
  const h = Math.floor(m / 60);
  const rm = m % 60;
  if (h < 24) return rm ? `${h}h ${rm}m` : `${h}h`;
  const d = Math.floor(h / 24);
  return `${d}d`;
}

/** Classify a fetch status into the screen's outcome vocabulary. */
export function classifyCredentialsStatus(status: number): Exclude<CredentialsOutcome, { kind: "ok" }>["kind"] {
  if (status >= 200 && status < 300) throw new Error("ok is not a failure kind");
  if (status === 401 || status === 403 || status === 404) return "not-found";
  if (status === 501) return "unconfigured";
  return "error";
}

// ── BYO service-account create form (ISI-3983) ────────────────────────────────
//
// The v1 credential WRITE path is a bring-your-own service-account key paste (POST
// /api/credentials, ISI-3679/ISI-3937). The (runtime, class) options mirror the credinject
// injection table (pkg/credinject): claude-code / openclaw / hermes carry a service-account
// key. The human-seat OAuth class is NOT paste-able — it is minted by the Connect Claude flow,
// which is blocked on a board Terms-of-Service decision (ISI-3661), and the apiserver answers a
// documented 501 for it, so it is deliberately absent from the paste form (the button above stays
// the honest, pending-board-approval seam for it).

/** A runtime offered by the paste form (mirror of api.RuntimeType* + the credinject table). */
export interface CreateRuntimeOption {
  value: string;
  label: string;
}

// ── Runtime credential entries (console mirror of pkg/credinject) ─────────────
//
// What a credential for each runtime INJECTS into the sandbox — env-var names verbatim
// from the reviewed injection table (pkg/credinject). The LLM settings surface reads this to
// describe the vibe entry honestly (P4.1, ISI-6158 C2): the per-user Mistral key maps onto
// MISTRAL_API_KEY and the BYO lane adds MISTRAL_BASE_URL (the unauthenticated Ollama-lane
// placeholder rides the same pair) — mirroring the claude-code/codex rows.

/** One runtime credential entry: the env a credential injects (+ the BYO base-URL env, if any). */
export interface RuntimeCredentialEntry {
  /** Runtime id — the exact api.RuntimeType* string POST /api/credentials validates. */
  runtime: string;
  /** The credential type surfaced on the runtime card ("mistral_api_key" for vibe). */
  credentialType?: string;
  /** The env var the injected credential materialises as in the sandbox. */
  injects: string;
  /** BYO lane: the optional base-URL env var riding the same entry. */
  byoInjects?: string;
}

/** The injection-contract mirror: claude-code/codex/opencode rows per pkg/credinject; vibe per P4.1 (ISI-6158). */
export const RUNTIME_CREDENTIAL_ENTRIES: readonly RuntimeCredentialEntry[] = [
  { runtime: "claude-code", injects: "ANTHROPIC_API_KEY" },
  { runtime: "codex", injects: "OPENAI_API_KEY" },
  { runtime: "openclaw", injects: "ANTHROPIC_API_KEY" },
  { runtime: "hermes", injects: "ANTHROPIC_API_KEY" },
  { runtime: "opencode", injects: "OPENAI_API_KEY" },
  {
    runtime: "vibe",
    credentialType: "mistral_api_key",
    injects: "MISTRAL_API_KEY",
    byoInjects: "MISTRAL_BASE_URL",
  },
];

/** Look up an entry by runtime id (undefined for a runtime outside the mirror). */
export function runtimeCredentialEntry(runtime: string): RuntimeCredentialEntry | undefined {
  return RUNTIME_CREDENTIAL_ENTRIES.find((e) => e.runtime === runtime);
}

/**
 * The service-account runtimes the injection table maps for a pasted key. vibe joins with the
 * P4.1 shim engine (ISI-6158): the pasted key injects as MISTRAL_API_KEY — the apiserver admits
 * the runtime once the host lane lands (ISI-6201); until then a paste surfaces the form's honest
 * server-error state.
 */
export const CREATE_RUNTIME_OPTIONS: CreateRuntimeOption[] = [
  { value: "claude-code", label: "Claude Code" },
  { value: "openclaw", label: "OpenClaw" },
  { value: "hermes", label: "Hermes" },
  { value: "vibe", label: "Vibe (Mistral)" },
];

/** The only class the paste form writes: a long-lived service-account key (never human-seat). */
export const CREATE_CLASS = "service-account";

/** The write body for POST /api/credentials. `value` is write-only (never serialised back out). */
export interface CredentialCreateInput {
  name: string;
  runtime: string;
  value: string;
  /** Fleet-admin routing hint (ISI-3937). Omitted for a bound single-team caller. */
  teamId?: string;
}

/**
 * Build the POST /api/credentials JSON body from the form input. Pure + unit-testable: trims the
 * name (a DNS-1123 Secret name), pins the service-account class, forwards the value verbatim, and
 * includes `teamId` ONLY when a fleet admin has picked a team (an empty hint is inert upstream).
 */
export function credentialCreateBody(input: CredentialCreateInput): Record<string, string> {
  const body: Record<string, string> = {
    name: input.name.trim(),
    runtime: input.runtime,
    class: CREATE_CLASS,
    value: input.value,
  };
  if (input.teamId) body.teamId = input.teamId;
  return body;
}

/** Outcome of a create POST, mapped to honest screen copy (status surfaced verbatim by the BFF). */
export type CreateOutcome =
  | "created" // 201 — Secret written, secretRef returned (never the value)
  | "select-team" // 400 — fleet admin must pick a team (ISI-3937 semantics)
  | "invalid" // 422 — field validation (name/runtime/value)
  | "conflict" // 409 — a credential with this name already exists
  | "denied" // 401/403/404 — deny/not-found collapse (existence-hiding)
  | "unsupported" // 501 — human-seat, provisioned by Connect Claude not a paste
  | "error"; // 5xx / network — the store itself is unhappy

export function classifyCreateStatus(status: number): CreateOutcome {
  if (status >= 200 && status < 300) return "created";
  if (status === 400) return "select-team";
  if (status === 409) return "conflict";
  if (status === 422) return "invalid";
  if (status === 501) return "unsupported";
  if (status === 401 || status === 403 || status === 404) return "denied";
  return "error";
}

/** One squad row for the fleet-admin team picker (mirror of the fleetlist TeamListEntry). */
export interface CredentialTeamOption {
  name: string;
  namespace: string;
  uid: string;
}

/** GET /api/squad/teams payload (subset the picker needs). `teams` may arrive null/absent. */
export interface CredentialTeamList {
  teams?: CredentialTeamOption[] | null;
  fleet?: boolean;
}
