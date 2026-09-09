// lib/projectSettings.ts — types + pure derivations + composed fetchers for the project Settings
// tab (ISI-4000 / S2).
//
// This module composes THREE existing endpoints — it adds no new backend:
//   1. GET  /api/projects/{id}/settings      (S1 read projection, projectsettings.go) — display.
//   2. GET  /api/squad/projects/{name}        (authoring spec, fleetlist.go/ProjectDetail) — the
//      write BASE. The compose PUT is a FULL authoring-spec REPLACE (composecrd.go upsert), so a
//      naive repo-only PUT would WIPE goals/egress/auth (the ISI-3985 empty-form hazard). We read
//      the current authoring spec and merge onto it so every write PRESERVES the untouched fields.
//   3. POST /api/credentials                  (secretwrite.go) — write the SCM PAT Secret.
//   4. PUT  /api/compose/projects/{name}       (composecrd.go planProject) — set repo.url/ref and/or
//      point spec.repo.auth.credentialSecretRef at the PAT Secret.
//   5. POST /api/projects/repo-auth/test       (repoauthtest.go) — server-side {ok,detail} probe.
//
// Integration pins discovered building this tab (all handled here, none needs a new route):
//   • KEY CONTRACT. POST /api/credentials stores a service-account paste under the injection-table
//     key "apiKey" (credinject) — NOT "token". The repo-sync reconciler (reposync tokenSecretKey)
//     and the repo-auth test BOTH honor an explicit credentialSecretRef.Key, defaulting to "token"
//     only when it is empty. So we reference the Secret with key "apiKey" everywhere it is read.
//   • CREATE-ONLY WRITE. POST /api/credentials is Create (409 on an existing name). "Replace" is
//     therefore a NEW unique Secret name + repoint the credentialSecretRef — the prior Secret is
//     left in place (no delete endpoint exists in v1). ponytail: orphaned prior PAT Secret — a
//     later slice owns credential GC; the repo now authenticates with the new Secret regardless.
//   • TOKEN IS WRITE-ONLY. The PAT is POSTed once and never read back: the projection only ever
//     knows the ref NAME + connected bool (S1 AC2). The screen clears the field on submit.
//
// PURE functions (classifyStatus / credentialStatusLabel / mergeProjectWrite / scmSecretName) carry
// the logic and are unit-tested without a DOM; the fetchers are thin async wrappers over the BFF.

import { credentialCreateBody } from "@/lib/credentials";

// ── Read projection types (mirror internal/apiserver/projectsettings.go) ──────

/** SCM projection of spec.repo (S1 AC1). `ref` empty ⇒ the provider default branch. */
export interface RepoSettings {
  url: string;
  ref: string;
  provider: string;
  syncEnabled: boolean;
  pollIntervalSeconds: number;
  reflectOutbound: boolean;
}

/** SCM credential state WITHOUT the token (S1 AC2). `lastTest` is the tri-state probe result. */
export interface AuthSettings {
  connected: boolean;
  credentialSecretRefName: string;
  lastTest: RepoTest;
}

/** The last-test tri-state — mirrors the RepoTest* constants (projectsettings.go). */
export type RepoTest = "passed" | "failed" | "untested";

export interface ProjectRef {
  name: string;
  namespace: string;
}

/** GET /api/projects/{id}/settings payload (mirror of apiserver ProjectSettings). */
export interface ProjectSettings {
  project: ProjectRef;
  repo: RepoSettings;
  auth: AuthSettings;
  canEdit: boolean;
}

// ── Read state machine (mirrors lib/github-status.ts GithubStatusState) ────────

export type SettingsState =
  | { kind: "loading" }
  | { kind: "unauthenticated" }
  | { kind: "not-found" } // 401/403/404 collapse — existence-hiding (never re-mapped)
  | { kind: "not-wired" } // apiserver's documented 501 (read model not wired in this deploy)
  | { kind: "error"; status: number }
  | { kind: "ready"; data: ProjectSettings };

/** Map an HTTP status onto the honest non-ok state it carries. 200 is handled by the fetcher. */
export function classifyStatus(status: number): Exclude<SettingsState, { kind: "ready" } | { kind: "loading" }> {
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

// ── Credential status label (S2 AC2) ──────────────────────────────────────────

export type StatusTone = "ok" | "bad" | "warn" | "idle";
export interface CredentialStatus {
  label: string;
  tone: StatusTone;
}

/**
 * The honest tri-state credential badge (AC2). The PRESENCE of a credential ref alone never
 * renders "healthy": a connected credential is qualified by its last-test result, and an
 * unconnected repo is "Not connected" regardless of anything else.
 */
export function credentialStatusLabel(connected: boolean, lastTest: RepoTest): CredentialStatus {
  if (!connected) return { label: "Not connected", tone: "idle" };
  switch (lastTest) {
    case "passed":
      return { label: "Connected — test passed", tone: "ok" };
    case "failed":
      return { label: "Connected — test failed", tone: "bad" };
    default:
      return { label: "Connected — untested", tone: "warn" };
  }
}

// ── Authoring-spec write base (mirror ProjectDetail / projectRequest wire) ─────

export interface WireSecretRef {
  name: string;
  key?: string;
}
export interface WireObjectRef {
  name: string;
  namespace?: string;
}
export interface RepoAuthWire {
  credentialSecretRef: WireSecretRef;
}
export interface RepoWire {
  url: string;
  ref?: string;
  auth?: RepoAuthWire;
}
/** GET /api/squad/projects/{name} authoring-spec projection (fleetlist.go ProjectDetail). */
export interface ProjectAuthoring {
  name: string;
  repo: RepoWire;
  goals?: string[];
  egressPolicyRef?: WireObjectRef | null;
}
/** The exact body the compose PUT (planProject) decodes — a FULL authoring-spec replace. */
export interface ProjectComposeBody {
  name: string;
  repo: RepoWire;
  goals?: string[];
  egressPolicyRef?: WireObjectRef;
}

/** A single-field patch onto the current authoring spec. Undefined fields are left untouched. */
export interface ProjectWritePatch {
  /** New repo URL (required by the CRD; omit to keep the current one). */
  url?: string;
  /** New tracked ref; "" means "default branch" and is omitted from the wire. */
  ref?: string;
  /** New/replacement credential ref (the PAT Secret just written). */
  credentialSecretRef?: WireSecretRef;
}

/**
 * Merge a patch onto the current authoring spec, producing the FULL compose PUT body. This is the
 * heart of the no-wipe guarantee: goals, egressPolicyRef, and any repo field NOT in the patch are
 * carried through verbatim so an edit of one facet never blanks another (ISI-3985). Pure +
 * unit-tested.
 */
export function mergeProjectWrite(base: ProjectAuthoring, patch: ProjectWritePatch): ProjectComposeBody {
  const url = patch.url !== undefined ? patch.url.trim() : base.repo.url;
  // ref: an explicit patch wins (empty ⇒ default branch ⇒ omitted); else keep the base ref.
  const ref = patch.ref !== undefined ? patch.ref.trim() : (base.repo.ref ?? "");
  // auth: an explicit patch repoints the credential; else preserve whatever is on the object.
  const auth: RepoAuthWire | undefined = patch.credentialSecretRef
    ? { credentialSecretRef: patch.credentialSecretRef }
    : base.repo.auth;

  const repo: RepoWire = { url };
  if (ref) repo.ref = ref;
  if (auth && auth.credentialSecretRef.name) repo.auth = auth;

  const body: ProjectComposeBody = { name: base.name, repo };
  if (base.goals && base.goals.length) body.goals = base.goals;
  if (base.egressPolicyRef && base.egressPolicyRef.name) body.egressPolicyRef = base.egressPolicyRef;
  return body;
}

// ── SCM PAT credential contract ───────────────────────────────────────────────

/**
 * The SCM PAT is stored through the shared service-account credential write. Every service-account
 * runtime maps to the "apiKey" data key (credinject table), so we pick one API-key runtime and
 * reference the resulting Secret with key "apiKey" (see the KEY CONTRACT note at the top).
 */
export const SCM_PAT_RUNTIME = "openclaw";
export const SCM_PAT_SECRET_KEY = "apiKey";

/**
 * A unique, DNS-1123 Secret name for a set/replace of the SCM PAT. Unique because the write is
 * create-only (409 on an existing name), so "replace" = a fresh Secret + repoint. `suffix` is
 * supplied by the caller (a base36 timestamp in the browser) to keep this pure + testable.
 */
export function scmSecretName(project: string, suffix: string): string {
  // Keep the whole name a valid DNS-1123 subdomain (<=253). The project CR name is already a
  // DNS-1123 label; a "-scm-" infix plus a lowercase base36 suffix stays in the alphabet.
  const stem = project.trim().toLowerCase().slice(0, 200);
  return `${stem}-scm-${suffix}`;
}

/** Build the POST /api/credentials body for an SCM PAT (reuses the shared service-account helper). */
export function scmCredentialBody(name: string, pat: string): Record<string, string> {
  return credentialCreateBody({ name, runtime: SCM_PAT_RUNTIME, value: pat });
}

// ── Field-error parsing (surface the apiserver's field errors VERBATIM — AC3) ──

/** The apiserver's 422 body: { error, fields: [{field, message}] }. */
export function parseFieldErrors(body: unknown): Record<string, string> {
  const out: Record<string, string> = {};
  const fields = (body as { fields?: unknown })?.fields;
  if (Array.isArray(fields)) {
    for (const f of fields) {
      const field = (f as { field?: unknown })?.field;
      const message = (f as { message?: unknown })?.message;
      if (typeof field === "string" && typeof message === "string") out[field] = message;
    }
  }
  return out;
}

// ── Fetchers (thin BFF wrappers; the apiserver stays the authority) ────────────

/** Read the S1 settings projection, returning the classified honest state (never fabricated). */
export async function fetchProjectSettings(projectId: string): Promise<SettingsState> {
  const res = await fetch(`/api/projects/${encodeURIComponent(projectId)}/settings`, {
    cache: "no-store",
  });
  if (res.ok) return { kind: "ready", data: (await res.json()) as ProjectSettings };
  return classifyStatus(res.status);
}

/** Read the current authoring spec (the write base). null when it cannot be read (surfaced as an error). */
export async function fetchProjectAuthoring(projectId: string): Promise<ProjectAuthoring | null> {
  const res = await fetch(`/api/squad/projects/${encodeURIComponent(projectId)}`, {
    cache: "no-store",
  });
  if (!res.ok) return null;
  return (await res.json()) as ProjectAuthoring;
}

export interface WriteOutcome {
  status: number;
  ok: boolean;
  /** Field errors surfaced verbatim from a 422 (AC3). */
  fields?: Record<string, string>;
}

/** PUT the merged authoring spec through the compose EDIT proxy. */
export async function putProjectCompose(projectId: string, body: ProjectComposeBody): Promise<WriteOutcome> {
  const res = await fetch(`/api/compose/projects/${encodeURIComponent(projectId)}`, {
    method: "PUT",
    headers: { "content-type": "application/json" },
    body: JSON.stringify(body),
    cache: "no-store",
  });
  if (res.ok) return { status: res.status, ok: true };
  const fields = res.status === 422 ? parseFieldErrors(await safeJson(res)) : undefined;
  return { status: res.status, ok: false, fields };
}

export interface CredentialWriteOutcome extends WriteOutcome {
  /** The Secret name written (echoed back on 201) so the caller can repoint the repo auth. */
  secretName?: string;
}

/** POST the SCM PAT Secret. Returns the written name so the caller can repoint repo.auth. */
export async function postScmCredential(name: string, pat: string): Promise<CredentialWriteOutcome> {
  const res = await fetch("/api/credentials", {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify(scmCredentialBody(name, pat)),
    cache: "no-store",
  });
  if (res.ok) {
    const body = (await safeJson(res)) as { name?: string } | null;
    return { status: res.status, ok: true, secretName: body?.name ?? name };
  }
  const fields = res.status === 422 ? parseFieldErrors(await safeJson(res)) : undefined;
  return { status: res.status, ok: false, fields };
}

export interface RepoAuthTestResult {
  status: number;
  /** Present on a 200 answer; on a non-200 the request itself failed (renders as an error). */
  ok?: boolean;
  detail?: string;
}

/**
 * Probe the STORED credential (AC5). The token never leaves the cluster — we send only {url,
 * credentialSecretRef}. `key` defaults to the SCM key contract so a repo connected by this tab is
 * probed under the same key the reconciler will later read.
 */
export async function testRepoAuth(
  url: string,
  ref: { name: string; key?: string },
): Promise<RepoAuthTestResult> {
  const res = await fetch("/api/projects/repo-auth/test", {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify({
      url,
      credentialSecretRef: { name: ref.name, key: ref.key ?? SCM_PAT_SECRET_KEY },
    }),
    cache: "no-store",
  });
  if (res.status === 200) {
    const body = (await safeJson(res)) as { ok?: boolean; detail?: string } | null;
    return { status: 200, ok: body?.ok, detail: body?.detail };
  }
  return { status: res.status };
}

/** Parse a response body as JSON, tolerating an empty/non-JSON body (returns null). */
async function safeJson(res: Response): Promise<unknown> {
  try {
    return await res.json();
  } catch {
    return null;
  }
}
