// lib/credentialsVault.ts — shared types + pure derivations for the WS-9 S4
// Project-settings Credentials Vault UI (ISI-5847). Mirrors lib/projectSettings.ts:
// thin fetch helpers + PURE, DOM-free derivations so the card stays a thin render
// and every rule is unit-testable (Vitest) without a DOM.
//
// Grounded on the MERGED backend (spec isi-5817 §1), NOT the design prose:
//   • read  — GET /api/projects/{id}/credential-types (internal/apiserver/credentialtypes.go)
//   • write — POST /api/credentials {name,value,type,projectId}  (vault axis,
//             internal/apiserver/secretwrite.go → createToolCredential)
//
// Acceptance-critical honesty rules carried here (spec §5/§8):
//   • the value is WRITE-ONLY — it rides one POST and is NEVER serialised back;
//     there is no GET verb for a value anywhere, so the client has no read path
//     to begin with. Nothing here ever returns or stores a typed value.
//   • "is it saved?" is read ONLY from gateState:"satisfied" — never inferred.
//   • rows ARE the consumable types (the intersection the backend already
//     computed); required-but-missing sorts FIRST (guided setup, not an error).

// ── Read model (mirror of apiserver CredentialTypeEntry / CredentialTypesResponse) ──

/** The resolved project the projection is scoped to (apiserver ProjectRef). */
export interface CredentialVaultProjectRef {
  name: string;
  namespace: string;
}

/** One consumable credential type the project's gated skills declare (one table
 *  row). Carries NO value and NO Secret name — "saved?" is gateState alone. */
export interface CredentialTypeEntry {
  /** Registry key = the POST {type} discriminator. */
  type: string;
  /** Dropdown/label text. */
  displayName: string;
  consumption: "header" | "env" | "file" | string;
  envVar?: string;
  headerName?: string;
  /** Informational only — the FE never sends it. */
  dataKey?: string;
  /** Gates go-live for the consuming skills (sorts to the top when unsatisfied). */
  requiredByGate: boolean;
  /** "none" ⇒ no connectivity probe is wired for this type. */
  probeKind: string;
  /** Skills/MCPServers that consume this type (chips). */
  consumedBy: string[];
  /** "satisfied" ⇒ already materialized; "unsatisfied" ⇒ not set. */
  gateState: "satisfied" | "unsatisfied" | string;
}

/** GET /api/projects/{id}/credential-types payload. `types` is always [] never
 *  null (apiserver contract) — the client never branches on null. */
export interface CredentialTypesResponse {
  project: CredentialVaultProjectRef;
  types: CredentialTypeEntry[];
}

/** Classified read outcome for the card (401/403/404 collapse = existence-hiding;
 *  501 = read model not wired on this deployment; honest frames, never a blank). */
export type CredentialTypesState =
  | { kind: "loading" }
  | { kind: "unauthenticated" }
  | { kind: "not-found" }
  | { kind: "not-wired" }
  | { kind: "error"; status: number }
  | { kind: "ready"; data: CredentialTypesResponse };

/** Map an HTTP status to the honest state it carries (mirrors classifyProjectSettings). */
export function classifyCredentialTypes(status: number): Exclude<CredentialTypesState, { kind: "ready" } | { kind: "loading" }> {
  switch (status) {
    case 401:
      return { kind: "unauthenticated" };
    case 403:
    case 404:
      return { kind: "not-found" };
    case 501:
      return { kind: "not-wired" };
    default:
      return { kind: "error", status };
  }
}

// ── Status badge derivation (spec §3 — gateState + requiredByGate, pure) ──────

export type VaultBadge = {
  label: string;
  tone: "ok" | "warn" | "bad" | "idle";
};

/**
 * Derive the row status badge from the read model ONLY (spec §3). No per-type
 * health projection exists yet (expired/untested are v1.1, spec §7), so v1 is:
 *   satisfied                      → Connected (ok)
 *   unsatisfied & requiredByGate   → Required  (bad — renders FIRST, §6)
 *   unsatisfied & !requiredByGate  → Not set   (idle — optional)
 * Presence is NEVER a guessed green: only an explicit "satisfied" is Connected.
 */
export function vaultStatusBadge(entry: CredentialTypeEntry): VaultBadge {
  if (entry.gateState === "satisfied") return { label: "Connected", tone: "ok" };
  if (entry.requiredByGate) return { label: "Required", tone: "bad" };
  return { label: "Not set", tone: "idle" };
}

/** True when the row is a required credential the project is still missing — the
 *  guided-setup rows that sort to the top (§6). */
export function isRequiredMissing(entry: CredentialTypeEntry): boolean {
  return entry.requiredByGate && entry.gateState !== "satisfied";
}

/** True when this type is already materialized (drives masked value + Rotate vs
 *  Create). Read ONLY from gateState — never from presence of any other field. */
export function isSatisfied(entry: CredentialTypeEntry): boolean {
  return entry.gateState === "satisfied";
}

/**
 * Order the rows for the table (spec §6): required-but-missing FIRST (turns the
 * go-live gate into guided setup), everything else after. Stable within each
 * group — the backend already sorts types alphabetically, so ties preserve that.
 * Returns a NEW array (never mutates the input).
 */
export function sortVaultTypes(types: CredentialTypeEntry[]): CredentialTypeEntry[] {
  return types
    .map((t, i) => ({ t, i }))
    .sort((a, b) => {
      const ra = isRequiredMissing(a.t) ? 0 : 1;
      const rb = isRequiredMissing(b.t) ? 0 : 1;
      if (ra !== rb) return ra - rb;
      return a.i - b.i; // stable: preserve backend order within a group
    })
    .map((x) => x.t);
}

/** The masked placeholder shown once a value is saved (spec §5 — there is NO
 *  reveal affordance; this is the ONLY representation of a stored value). */
export const MASKED_VALUE = "••••••••";

// ── Write path (POST /api/credentials vault axis — spec §1.2) ─────────────────

/** The write body for POST /api/credentials on the vault axis. `value` is
 *  write-only: it rides one request and is never serialised back out. `name` is
 *  carried for wire-completeness (spec §1.2 body) — the apiserver DERIVES the
 *  Secret name from (project, type) and ignores this field on the vault axis, so
 *  it is NOT an editable row field (see the card; this avoids a dishonest
 *  "rename" affordance that the backend would silently drop). */
export interface ToolCredentialCreateInput {
  name: string;
  value: string;
  type: string;
  projectId: string;
}

/** Build the POST /api/credentials vault-axis JSON body. Pure + unit-testable:
 *  forwards the value verbatim (write-only), pins the {type} discriminator and
 *  projectId that route the write through the binding registry into the
 *  project's namespace. */
export function toolCredentialCreateBody(input: ToolCredentialCreateInput): Record<string, string> {
  return {
    name: input.name,
    value: input.value,
    type: input.type,
    projectId: input.projectId,
  };
}

/** Outcome of a vault-axis create/rotate POST, mapped to honest card copy. The
 *  apiserver surfaces its detail verbatim (E5 discipline); the card renders it. */
export type VaultCreateOutcome =
  | "created" // 201 — Secret written, secretRef returned (never the value) → mask
  | "conflict" // 409 — a credential of this type already exists → the Rotate path (§4, Slice B)
  | "invalid" // 400/422 — empty value / unmapped type → inline field error
  | "denied" // 401/403 — unauthenticated / write-tier denied (row should not have been editable)
  | "unsupported" // 501 — human-seat (OAuth) type / axis unavailable on this host
  | "error"; // 502 / 5xx / network — the store itself is unhappy

/** Classify the POST status into the card's outcome vocabulary (spec §1.2 table). */
export function classifyVaultCreateStatus(status: number): VaultCreateOutcome {
  if (status >= 200 && status < 300) return "created";
  if (status === 409) return "conflict";
  if (status === 400 || status === 422) return "invalid";
  if (status === 401 || status === 403) return "denied";
  if (status === 501) return "unsupported";
  return "error";
}

/** Human copy for an outcome (generic fallbacks; a verbatim apiserver
 *  `detail`/`fields` message, when present, takes precedence at the call site).
 *  "created" never reaches a message site (the 201 path masks instead) but is
 *  handled so the switch is exhaustive. */
export function vaultCreateMessage(outcome: VaultCreateOutcome): string {
  switch (outcome) {
    case "created":
      return "Saved.";
    case "conflict":
      return "A credential of this type already exists — use Rotate to replace it.";
    case "invalid":
      return "Paste a value for this credential.";
    case "denied":
      return "You don't have permission to set this credential.";
    case "unsupported":
      return "This type is provisioned by Connect Claude, not a pasted key.";
    case "error":
      return "Couldn't save — try again.";
  }
}

/** Parse the apiserver error body ({error?, detail?, fields?}) into one verbatim
 *  line (E5 discipline — show the server's field message, never a generic toast).
 *  Returns null when the body carries nothing renderable (caller falls back to
 *  the generic outcome copy). Deliberately tolerant of a non-JSON body. */
export async function vaultErrorText(res: Response): Promise<string | null> {
  try {
    const j = (await res.json()) as {
      error?: string;
      detail?: string;
      fields?: Array<{ field: string; message: string }>;
    };
    if (Array.isArray(j.fields) && j.fields.length) {
      return j.fields.map((f) => `${f.field}: ${f.message}`).join("; ");
    }
    if (j.detail) return j.detail;
    if (j.error) return j.error;
  } catch {
    /* non-JSON body — fall through */
  }
  return null;
}

// ── Fetch helpers (mirror lib/projectSettings.ts: thin, cookie-forwarded BFF) ──

/** Read the credential-types projection through the BFF. 200 ⇒ ready; else the
 *  classified honest state. Never throws on a non-2xx — a transport throw is the
 *  caller's `error` frame. */
export async function fetchCredentialTypes(projectId: string): Promise<CredentialTypesState> {
  const res = await fetch(`/api/projects/${encodeURIComponent(projectId)}/credential-types`, {
    cache: "no-store",
  });
  if (res.ok) {
    const data = (await res.json()) as CredentialTypesResponse;
    // The apiserver guarantees types:[] (never null); defend anyway so a
    // malformed 200 renders the honest empty card, never a crash.
    if (!Array.isArray(data.types)) data.types = [];
    return { kind: "ready", data };
  }
  return classifyCredentialTypes(res.status);
}

/** Create/materialize a vault credential (POST /api/credentials vault axis). The
 *  value is write-only — it rides one request and is never serialised back.
 *  Returns the raw Response so the caller classifies (created / conflict /
 *  invalid / …) and surfaces the apiserver's field message verbatim. */
export async function createToolCredential(input: ToolCredentialCreateInput): Promise<Response> {
  return fetch("/api/credentials", {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify(toolCredentialCreateBody(input)),
    cache: "no-store",
  });
}
