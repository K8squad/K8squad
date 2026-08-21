// lib/audit.ts — shared types + pure derivations for the Audit trail screen (story 2.6 / ISI-2881).
//
// The data contract mirrors the apiserver's GET /api/audit projection
// (internal/apiserver/audittrail.go). Presentation derivations are PURE functions so the
// component stays a thin render and the derivations are unit-testable (Vitest) without a DOM.
//
// Honesty rules carried from the house pattern (8.6): the deny collapse (401/403) renders a
// denied state — with the specific hint that non-admin queries are self-scoped — the 501
// (reader not wired) renders an explicit unconfigured state, and nothing is ever fabricated.

/** One immutable coord.audit_log row (mirror of the apiserver's AuditEvent). */
export interface AuditEvent {
  id: number;
  workItemId?: string | null;
  runId?: string | null;
  eventType: string;
  principal: string;
  initiatedByUserId?: string | null;
  fenceToken?: number | null;
  fromState?: string | null;
  toState?: string | null;
  payload?: unknown;
  createdAt: string;
}

/** GET /api/audit page (mirror of the apiserver's AuditTrailPage): newest-first events + the
 *  id-cursor for the next older page (null ⇒ tail). */
export interface AuditPage {
  events: AuditEvent[];
  nextBefore: number | null;
}

/** The event-type vocabulary the coord writers emit today (0001 comment + the pkg/coord prod
 *  writers + the Epic-15 admin sink). The filter offers it as a closed list for convenience;
 *  the server accepts any string (forward-compatible), and rows of unseen types still render. */
export const AUDIT_EVENT_TYPES = [
  "claim_acquired",
  "claim_renewed",
  "claim_released",
  "comment_added",
  "artifact_registered",
  "state_transition",
  "reconcile_advanced",
  "coordinator_dispatched",
  "run_terminal",
  "completed",
  "user_created",
  "user_updated",
  "user_deactivated",
] as const;

/** Filter form state — one field per query param the API accepts. */
export interface AuditFilters {
  workItem: string;
  run: string;
  actor: string;
  eventType: string;
  from: string; // datetime-local input value ("YYYY-MM-DDTHH:mm") or ""
  to: string;
}

export const emptyAuditFilters: AuditFilters = {
  workItem: "",
  run: "",
  actor: "",
  eventType: "",
  from: "",
  to: "",
};

/** Serialize filters into the API's query string. datetime-local values (naive local time)
 *  are converted to UTC RFC3339 — the server parses strict RFC3339. `before` and `limit` ride
 *  pagination, not the filter form. */
export function auditQueryString(f: AuditFilters, before?: number | null, limit = 50): string {
  const params = new URLSearchParams();
  if (f.workItem.trim()) params.set("work_item", f.workItem.trim());
  if (f.run.trim()) params.set("run", f.run.trim());
  if (f.actor.trim()) params.set("actor", f.actor.trim());
  if (f.eventType) params.set("event_type", f.eventType);
  const fromMs = Date.parse(f.from);
  if (!Number.isNaN(fromMs)) params.set("from", new Date(fromMs).toISOString());
  const toMs = Date.parse(f.to);
  if (!Number.isNaN(toMs)) params.set("to", new Date(toMs).toISOString());
  if (before != null) params.set("before", String(before));
  params.set("limit", String(limit));
  return params.toString();
}

/** Classified fetch outcome for the screen — 401/404 collapse (existence-hiding); a 403 stays
 *  DISTINCT because the server uses it for exactly one honest thing here: a non-admin querying
 *  a foreign actor. That deserves its own message, not a generic deny. */
export type AuditOutcome =
  | { kind: "ok" }
  | { kind: "denied" } // 401/404 — unauthenticated or no surface for this caller
  | { kind: "forbidden-actor" } // 403 — non-admin querying a foreign actor
  | { kind: "unconfigured" } // 501 — read model not wired (reader-less host shape)
  | { kind: "error" }; // 5xx / network

export function classifyAuditStatus(status: number): Exclude<AuditOutcome, { kind: "ok" }>["kind"] {
  if (status >= 200 && status < 300) throw new Error("ok is not a failure kind");
  if (status === 403) return "forbidden-actor";
  if (status === 401 || status === 404) return "denied";
  if (status === 501) return "unconfigured";
  return "error";
}

/** Event badge tone: state-changing coordination events read as activity, terminal ones as
 *  outcomes, admin mutations as governance, everything else as neutral. */
export type EventBadge = { label: string; tone: "info" | "ok" | "warn" | "idle" };

const badgeByType: Record<string, { label: string; tone: EventBadge["tone"] }> = {
  claim_acquired: { label: "Checkout", tone: "info" },
  claim_renewed: { label: "Renew", tone: "info" },
  claim_released: { label: "Release", tone: "info" },
  comment_added: { label: "Comment", tone: "idle" },
  artifact_registered: { label: "Artifact", tone: "ok" },
  state_transition: { label: "Transition", tone: "warn" },
  reconcile_advanced: { label: "Reconcile", tone: "idle" },
  coordinator_dispatched: { label: "Dispatch", tone: "info" },
  run_terminal: { label: "Run final", tone: "ok" },
  completed: { label: "Completed", tone: "ok" },
  user_created: { label: "User +", tone: "warn" },
  user_updated: { label: "User ~", tone: "warn" },
  user_deactivated: { label: "User −", tone: "warn" },
};

export function eventBadge(e: AuditEvent): EventBadge {
  return badgeByType[e.eventType] ?? { label: e.eventType, tone: "idle" };
}

/** Result cell: the transition (from → to) when present, else the first useful payload scalar,
 *  else the honest em dash — never a fabricated summary. */
export function resultLabel(e: AuditEvent): string {
  if (e.fromState || e.toState) {
    return `${e.fromState ?? "—"} → ${e.toState ?? "—"}`;
  }
  if (e.payload && typeof e.payload === "object" && !Array.isArray(e.payload)) {
    const p = e.payload as Record<string, unknown>;
    for (const key of ["detail", "reason", "result", "to", "state"]) {
      const v = p[key];
      if (typeof v === "string" && v) return v;
      if (typeof v === "number" || typeof v === "boolean") return String(v);
    }
  }
  return "—";
}

/** "21 Aug 09:56" — the table's compact when; the title attribute carries the full instant. */
export function whenLabel(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "—";
  return d.toLocaleString(undefined, {
    day: "2-digit",
    month: "short",
    hour: "2-digit",
    minute: "2-digit",
  });
}

/** Principal cell: "user:jane" → "jane (user)"; "agent:coder" → "coder (agent)"; unknown shapes
 *  render verbatim. Human-vs-agent is derived from the prefix, matching how the discussion
 *  surface derives it (never a stored flag). */
export function actorLabel(principal: string): string {
  if (principal.startsWith("user:")) return `${principal.slice(5)} (user)`;
  if (principal.startsWith("agent:")) return `${principal.slice(6)} (agent)`;
  return principal;
}

/** Short "…abc123" form of a uuid for the work-item / run cells (title carries the full id). */
export function shortId(id?: string | null): string {
  if (!id) return "—";
  return id.length > 8 ? "…" + id.slice(-8) : id;
}
