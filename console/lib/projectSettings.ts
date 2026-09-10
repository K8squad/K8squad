// lib/projectSettings.ts — ISI-4000 S2: the project-Settings tab payload types +
// pure derivations + the browser-side composers for the three EXISTING endpoints
// the tab drives.
//
// Types mirror internal/apiserver/projectsettings.go (S1) EXACTLY — the Go struct
// is the contract owner. The tab composes, it never invents: it READS the S1
// projection (GET /api/projects/{id}/settings), and WRITES only through the
// endpoints that already exist —
//   • PUT  /api/compose/projects/{name}   (planProject → spec.repo.{url,ref,auth})
//   • POST /api/credentials                (BYO Secret write, ISI-3937)
//   • POST /api/projects/repo-auth/test    (server-side probe, repoauthtest.go)
//
// The SCM PAT is WRITE-ONLY: the projection only ever knows the ref NAME + a
// `connected` bool + the tri-state last-test (S1 AC2 — no token ever crosses the
// wire). This module has no field that could carry token material back out.

import { credentialCreateBody, type CredentialCreateInput } from "@/lib/credentials";

// ── S1 read projection (mirror of projectsettings.go) ─────────────────────────

/** spec.repo projection (S1 RepoSettings). Ref "" ⇒ the tab renders "default branch". */
export interface RepoSettings {
  url: string;
  ref: string;
  provider: string;
  syncEnabled: boolean;
  pollIntervalSeconds: number;
  reflectOutbound: boolean;
}

/** SCM credential state WITHOUT the token (S1 AuthSettings). `lastTest` is the
 *  tri-state "passed" | "failed" | "untested" — presence of a ref alone is NOT
 *  "healthy" (AC2). */
export interface AuthSettings {
  connected: boolean;
  credentialSecretRefName: string;
  lastTest: string;
}

/** GET /api/projects/{id}/settings payload (S1 ProjectSettings). Carries NO
 *  token/Secret data field — the no-token invariant is structural. */
export interface ProjectSettings {
  project: { name: string; namespace: string };
  repo: RepoSettings;
  auth: AuthSettings;
  canEdit: boolean;
}

/** The distinct honest state an HTTP status carries (mirrors GithubStatusState).
 *  501 ⇒ the S1 read model is not wired in this deployment — the tab renders
 *  "settings not available yet", never a blank form or fabricated values. */
export type ProjectSettingsState =
  | { kind: "loading" }
  | { kind: "unauthenticated" }
  | { kind: "not-found" }
  | { kind: "not-wired" }
  | { kind: "error"; status: number }
  | { kind: "ready"; data: ProjectSettings };

/** Map an HTTP status to the state it carries (401/404 collapse = existence-hiding). */
export function classifyProjectSettings(status: number): ProjectSettingsState {
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

// ── AC2: honest tri-state credential badge (pure, unit-testable, no DOM) ───────

export type CredentialBadge = {
  label: string;
  tone: "ok" | "warn" | "bad" | "idle";
};

/**
 * Derive the credential status row from S1's `auth.{connected, lastTest}` (AC2).
 * The four honest outcomes: no credential ⇒ "Not connected"; a connected ref is
 * NEVER rendered "healthy" on presence alone — it reads "passed" / "failed" /
 * "untested" strictly off `lastTest`. Any unexpected lastTest collapses to
 * "untested" (honesty over a guessed-green).
 */
export function credentialStatusLabel(connected: boolean, lastTest: string): CredentialBadge {
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

// ── The compose wire (mirror of ProjectDetail / projectRequest) ────────────────
//
// A compose PUT is a FULL-SPEC upsert (composecrd.go upsert() replaces the spec
// built from the request — it does not merge onto the live object). So a Settings
// write MUST round-trip the WHOLE authoring spec: we read the current ProjectDetail
// (GET /api/squad/projects/{name}) and overlay only the fields the tab owns
// (repo.url / repo.ref / repo.auth), preserving goals + egressPolicyRef so a
// repo-URL save never silently wipes them (v1 leaves those read-only).

export interface WireSecretRef {
  name: string;
  key?: string;
}

/** ProjectDetail authoring wire (mirror of fleetlist.go ProjectDetail). */
export interface ProjectDetail {
  name: string;
  repo: {
    url: string;
    ref?: string;
    auth?: { credentialSecretRef: WireSecretRef } | null;
  };
  goals?: string[];
  egressPolicyRef?: { name: string; namespace?: string } | null;
}

/**
 * The (runtime, class) the SCM-PAT Secret is written under via POST /api/credentials.
 *
 * ponytail: the credential write path (secretwrite.go) picks the Secret data key
 * from `credinject.DefaultSecretKey(runtime, class)` — for a service-account it is
 * "apiKey", NOT "token". The repo-auth test + repo-sync reconciler both read
 * `credentialSecretRef.key`, defaulting to "token" when empty. So the tab MUST pin
 * the ref key to the key the write actually used ("apiKey"), or the probe/sync
 * would read an empty "token" and fail. This couples the console to the credinject
 * table; upgrade path = a dedicated SCM-credential class whose default key is
 * "token" (tracked for the board, out of this v1's scope).
 */
export const SCM_PAT_RUNTIME = "claude-code";
export const SCM_PAT_SECRET_KEY = "apiKey";

/**
 * Build the full compose PUT body from the current detail + the tab's overlay.
 * Preserves goals + egressPolicyRef (full-replace safety); overlays repo.url/ref;
 * carries repo.auth from either an explicit new ref (PAT just attached) or the
 * detail's existing auth (so a repo-only edit never drops a connected credential).
 */
export function buildProjectPutBody(
  detail: ProjectDetail,
  overlay: { repoUrl: string; repoRef?: string; credentialSecretRef?: WireSecretRef },
): Record<string, unknown> {
  const repo: Record<string, unknown> = { url: overlay.repoUrl.trim() };
  const ref = (overlay.repoRef ?? detail.repo.ref ?? "").trim();
  if (ref) repo.ref = ref;

  const authRef = overlay.credentialSecretRef ?? detail.repo.auth?.credentialSecretRef;
  if (authRef && authRef.name) {
    repo.auth = {
      credentialSecretRef: authRef.key
        ? { name: authRef.name, key: authRef.key }
        : { name: authRef.name },
    };
  }

  const body: Record<string, unknown> = { name: detail.name, repo };
  if (detail.goals && detail.goals.length) body.goals = detail.goals;
  if (detail.egressPolicyRef && detail.egressPolicyRef.name) body.egressPolicyRef = detail.egressPolicyRef;
  return body;
}

// ── repo-auth test (mirror of repoauthtest.go) ────────────────────────────────

/** POST /api/projects/repo-auth/test response — rendered VERBATIM (E5 discipline);
 *  an honest `{ok:false}` is a valid, expected outcome, never rewritten to success. */
export interface RepoAuthTestResult {
  ok: boolean;
  detail: string;
}

// ── Browser-side composers over the BFF choke point ────────────────────────────

/** Read the S1 projection through the BFF. 200 ⇒ ready; else the classified honest state. */
export async function fetchProjectSettings(projectId: string): Promise<ProjectSettingsState> {
  const res = await fetch(`/api/projects/${encodeURIComponent(projectId)}/settings`, {
    cache: "no-store",
  });
  if (res.ok) return { kind: "ready", data: (await res.json()) as ProjectSettings };
  return classifyProjectSettings(res.status);
}

/** Read the full authoring detail (goals/egress/auth) so a compose PUT round-trips
 *  the whole spec. Null ⇒ the detail read failed (caller surfaces an honest error). */
export async function fetchProjectDetail(projectId: string): Promise<ProjectDetail | null> {
  const res = await fetch(`/api/squad/projects/${encodeURIComponent(projectId)}`, {
    cache: "no-store",
  });
  if (!res.ok) return null;
  return (await res.json()) as ProjectDetail;
}

/** PUT the composed project spec. Returns the raw Response so the caller can
 *  surface the apiserver's field errors (422) / conflict (409) VERBATIM. */
export async function putProject(
  projectId: string,
  body: Record<string, unknown>,
): Promise<Response> {
  return fetch(`/api/compose/projects/${encodeURIComponent(projectId)}`, {
    method: "PUT",
    headers: { "content-type": "application/json" },
    body: JSON.stringify(body),
    cache: "no-store",
  });
}

/** Write/replace the SCM PAT Secret (POST /api/credentials). The value is
 *  write-only — it rides one request and is never serialised back. Returns the
 *  raw Response so the caller classifies (created / conflict / invalid / …). */
export async function createScmCredential(
  input: Pick<CredentialCreateInput, "name" | "value" | "teamId">,
): Promise<Response> {
  const body = credentialCreateBody({
    name: input.name,
    runtime: SCM_PAT_RUNTIME,
    value: input.value,
    ...(input.teamId ? { teamId: input.teamId } : {}),
  });
  return fetch("/api/credentials", {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify(body),
    cache: "no-store",
  });
}

/** Run the server-side repo-auth probe. Returns the {ok, detail} verbatim (AC5);
 *  a transport throw is surfaced by the caller as a retryable error, never as a
 *  fabricated success. */
export async function testRepoAuth(
  url: string,
  credentialSecretRef: WireSecretRef,
): Promise<RepoAuthTestResult> {
  const res = await fetch("/api/projects/repo-auth/test", {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify({
      url,
      credentialSecretRef: credentialSecretRef.key
        ? { name: credentialSecretRef.name, key: credentialSecretRef.key }
        : { name: credentialSecretRef.name },
    }),
    cache: "no-store",
  });
  if (res.ok) return (await res.json()) as RepoAuthTestResult;
  // A non-2xx from the probe surface is itself an honest failure to render.
  return { ok: false, detail: `test-connection request failed (HTTP ${res.status})` };
}
