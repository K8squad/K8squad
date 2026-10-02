"use client";

// lib/agents/rosterEdit.ts — the inline-edit write model for the Agents & Team redesign
// (ISI-5362 / S5, Frame 02; epic ISI-5357; design ISI-5305 §2–§3).
//
// The read-only roster (lib/agents/roster.ts) consumes the fleet LIST reads; THIS module adds
// the gated-then-ungated edit path on top of the two backend stories that have now landed:
//   • ISI-5358 (S1) — the Role authoring read round-trips model/fallback/phase fields, and the
//     role PUT decode is strict (an unknown field is a loud 400, never a silent wipe).
//   • ISI-5359 (S2) — the compose PUT is a FIELD-SCOPED MERGE (RFC 7386 restricted to top-level
//     spec fields): a sent field overwrites, JSON-null deletes, every unsent live field rides
//     through untouched. That is what makes "send only what changed" safe here.
//   • ISI-5361 (S4) — RoleDetail carries resourceVersion (opaque concurrency token) and the
//     reverse usedBy index that powers the blast-radius confirm.
//
// The wire builders are PURE and unit-tested: each returns the exact minimal body for one
// inline commit — the admission-required identity fields of its kind plus ONLY the edited
// field(s). Nothing else is sent, so the merge cannot drop a field the form never saw (the
// failure mode the S1+S2 gate existed to prevent).

// ── Wire shapes (verbatim from internal/apiserver fleetlist.go / composecrd.go) ──

export interface ObjectRefWire {
  name: string;
  namespace?: string;
}

export interface SecretRefWire {
  name: string;
  key?: string;
}

export interface FallbackModelWire {
  model: string;
  modelEndpointRef?: SecretRefWire | null;
}

/** GET /api/squad/agents/{name} — the authoring-spec projection (ADR-0016). */
export interface AgentDetailWire {
  name: string;
  runtimeRef: ObjectRefWire;
  roleRef: ObjectRefWire;
  skillRefs?: ObjectRefWire[];
  model: string;
  modelEndpointRef?: SecretRefWire | null;
  credentialSecretRef: SecretRefWire;
  credentialClass?: string;
  fallbackModel?: FallbackModelWire | null;
}

/** GET /api/squad/roles/{name} — the authoring-spec projection (ISI-5358/5361-complete). */
export interface RoleDetailWire {
  name: string;
  promptRef: ObjectRefWire;
  defaultSkills?: ObjectRefWire[];
  runtimeClassHint?: string;
  model?: string;
  fallbackModel?: FallbackModelWire | null;
  activePhases?: string[];
  coordinator?: boolean;
  coordinatorMode?: string;
  /** Opaque concurrency token (ISI-5361 Gap 6) — round-tripped on the PUT, never parsed. */
  resourceVersion?: string;
  /** Reverse reference index (ISI-5361 Gap 7) — the blast radius of a shared-role edit. */
  usedBy: { agents: string[]; roles?: string[] };
}

// ── Detail reads ─────────────────────────────────────────────────────────────

async function getJson<T>(path: string, signal: AbortSignal): Promise<T> {
  const res = await fetch(path, {
    headers: { accept: "application/json" },
    cache: "no-store",
    signal,
  });
  if (res.status === 404) throw new RosterEditError("not-found", "That item is no longer in the roster.");
  if (!res.ok) throw new RosterEditError("read-failed", `Could not load ${path} (${res.status}).`);
  return (await res.json()) as T;
}

function teamQS(team?: string): string {
  return team ? `?team=${encodeURIComponent(team)}` : "";
}

/** Fetch one agent's full authoring spec — the required-field round-trip source for edits. */
export function fetchAgentDetail(name: string, signal: AbortSignal, team?: string): Promise<AgentDetailWire> {
  return getJson<AgentDetailWire>(`/api/squad/agents/${encodeURIComponent(name)}${teamQS(team)}`, signal);
}

/** Fetch one role's full authoring spec (model, fallback, usedBy, resourceVersion). */
export function fetchRoleDetail(name: string, signal: AbortSignal, team?: string): Promise<RoleDetailWire> {
  return getJson<RoleDetailWire>(`/api/squad/roles/${encodeURIComponent(name)}${teamQS(team)}`, signal);
}

// ── Save ladder (Frame 04: Idle → Editing → Saving → Saved / Error) ──────────

export type SaveState =
  | { kind: "idle" }
  | { kind: "saving" }
  | { kind: "saved" }
  | { kind: "error"; message: string };

/** A structured save failure: `status` carries the HTTP code for the 409 staleness copy. */
export class RosterEditError extends Error {
  readonly code: "not-found" | "read-failed" | "conflict" | "rejected" | "scope";
  readonly status: number;
  constructor(code: RosterEditError["code"], message: string, status = 0) {
    super(message);
    this.code = code;
    this.status = status;
  }
}

/**
 * PUT one inline commit through the BFF compose route. The body is the minimal wire from a
 * builder below; the apiserver merges it field-scoped (ISI-5359). Errors are parsed into the
 * honest ladder shapes: 409 ⇒ conflict ("changed since you opened — reload"), 400 project
 * scope ⇒ scope (the admin-vs-tenant seam), 422 ⇒ rejected with the server's field message.
 */
export async function putCompose(
  kind: "agents" | "roles",
  name: string,
  body: Record<string, unknown>,
): Promise<void> {
  const res = await fetch(`/api/compose/${kind}/${encodeURIComponent(name)}`, {
    method: "PUT",
    headers: { "content-type": "application/json" },
    body: JSON.stringify(body),
  });
  if (res.ok) return;
  let message = `Save failed (status ${res.status}).`;
  try {
    const j = JSON.parse(await res.text()) as { error?: string; fields?: Array<{ field: string; message: string }> };
    if (j.error) message = j.error;
    if (Array.isArray(j.fields) && j.fields.length) {
      message = `${message} (${j.fields.map((f) => `${f.field}: ${f.message}`).join("; ")})`;
    }
  } catch {
    /* non-JSON body — keep the status line */
  }
  if (res.status === 409) throw new RosterEditError("conflict", "Changed since you opened — reload and retry.", 409);
  if (res.status === 400 && /project scope/i.test(message)) {
    throw new RosterEditError(
      "scope",
      "Inline writes need an admin scope on this surface. Non-admins: edit via Compose with a project scope.",
      400,
    );
  }
  throw new RosterEditError("rejected", message, res.status);
}

// ── Pure wire builders — each sends ONLY the admission-required identity of its
//    kind plus the edited field(s); the merge keeps everything else (ISI-5359). ──

/**
 * The required round-trip base for a role PUT: planRole validates name + promptRef.name
 * unconditionally, so every role commit — even a one-field model edit — carries them.
 * resourceVersion (ISI-5361) rides along opaquely so the server sees the token the editor
 * read (tolerated-and-dropped today; the sent-map keeps the seam forward-compatible).
 */
function roleBase(detail: RoleDetailWire): Record<string, unknown> {
  const body: Record<string, unknown> = {
    name: detail.name,
    promptRef: { name: detail.promptRef.name },
  };
  if (detail.resourceVersion) body.resourceVersion = detail.resourceVersion;
  return body;
}

/**
 * The required round-trip base for an agent PUT: planAgent validates runtimeRef.name,
 * roleRef.name and credentialSecretRef.name unconditionally. `model` is deliberately NOT
 * round-tripped — a blank primary means inherit (ISI-4892) and the merge carries the live
 * value untouched when the edit does not touch it.
 */
function agentBase(detail: AgentDetailWire): Record<string, unknown> {
  return {
    name: detail.name,
    runtimeRef: { name: detail.runtimeRef.name },
    roleRef: { name: detail.roleRef.name },
    credentialSecretRef: { name: detail.credentialSecretRef.name },
  };
}

export interface RoleModelEdit {
  /** "" ⇒ clear the role-tier pin (JSON-null delete ⇒ inherit the org default). */
  model: string;
  /** undefined ⇒ untouched (not sent); "" ⇒ clear; else the fallback model id. */
  fallbackModel: string | undefined;
}

/** Role → Model tab commit: sets/clears the role-tier model (and optional fallback). */
export function roleModelPut(detail: RoleDetailWire, edit: RoleModelEdit): Record<string, unknown> {
  const body = roleBase(detail);
  body.model = edit.model.trim() ? edit.model.trim() : null; // null ⇒ RFC 7386 delete ⇒ inherit
  if (edit.fallbackModel !== undefined) {
    body.fallbackModel = edit.fallbackModel.trim() ? { model: edit.fallbackModel.trim() } : null;
  }
  return body;
}

/** Role → Default Skills commit: the full replacement array ([] ⇒ JSON-null clear). */
export function roleSkillsPut(detail: RoleDetailWire, skills: string[]): Record<string, unknown> {
  const body = roleBase(detail);
  const unique = [...new Set(skills.map((s) => s.trim()).filter(Boolean))];
  // An empty array must be the null sentinel: planRole omits an empty DefaultSkills on the
  // authored side, so a bare [] would be a no-op merge instead of a clear.
  body.defaultSkills = unique.length ? unique.map((name) => ({ name })) : null;
  return body;
}

/** Agent → Model tab commit: sets the agent-tier override, or clears it (null ⇒ inherit). */
export function agentModelPut(detail: AgentDetailWire, model: string): Record<string, unknown> {
  const body = agentBase(detail);
  body.model = model.trim() ? model.trim() : null;
  return body;
}

/** Agent → Skills tab commit: the full replacement skillRefs array ([] ⇒ JSON-null clear). */
export function agentSkillsPut(detail: AgentDetailWire, skills: string[]): Record<string, unknown> {
  const body = agentBase(detail);
  const unique = [...new Set(skills.map((s) => s.trim()).filter(Boolean))];
  body.skillRefs = unique.length ? unique.map((name) => ({ name })) : null;
  return body;
}

/**
 * Effective skills of an agent = role.defaultSkills ∪ agent.skillRefs (ISI-5305 §4 — the union,
 * never conflation). Pure partition for the provenance-split chips (Frame 03): role-tier skills
 * first (non-removable here), then agent-added ones (removable), de-duplicated across tiers.
 */
export function partitionAgentSkills(
  agentDetail: AgentDetailWire,
  roleDefaultSkills: string[],
): { fromRole: string[]; addedToAgent: string[] } {
  const fromRole = [...new Set(roleDefaultSkills.map((s) => s.trim()).filter(Boolean))];
  const fromRoleSet = new Set(fromRole);
  const addedToAgent = [
    ...new Set((agentDetail.skillRefs ?? []).map((r) => r.name.trim()).filter(Boolean)),
  ].filter((s) => !fromRoleSet.has(s));
  return { fromRole, addedToAgent };
}
