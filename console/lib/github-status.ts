// lib/github-status.ts — ISI-3956 S5c: the GitHub-status payload types + the
// browser-side fetcher for the BFF route.
//
// Types mirror internal/apiserver/githubstatus.go EXACTLY; the Go structs are
// the contract owner. The tab reads the scm MIRROR (never GitHub), so freshness
// is honest — the response carries the mirror's own timestamps, which the tab
// renders as "synced Ns ago", NEVER a fabricated "live" badge (ADR-0013 §D4).
// The BYO repo credential is never present (the apiserver never returns it).

/** The OPTIONAL local automated-review block (ISI-4767 / ISI-4750 E5), mirroring
 * internal/apiserver/githubstatus.go GithubPRReview EXACTLY. Present only when the
 * PR is bridged to a system-dispatched PR-review work item (label
 * `ksquad.github.pr=<owner>/<repo>#N`, produced by E4). ABSENT means "not
 * reviewed" — the honest default (ADR-0013 §D4), never a fabricated verdict. It is
 * a LOCAL work-item status, NOT a GitHub review, and must never merge into
 * `reviewState` (the raw provider column). MVP surfaces presence + a deep-link to
 * the work item; the reviewing agent name + reviewed head-SHA are Phase-2. */
export type GithubPRReview = {
  workItemId: string;
  /** The review work item's coord state (backlog | in progress | done | …). */
  state?: string;
};

export type GithubPR = {
  number: number;
  title: string;
  state: string;
  reviewState?: string;
  merged?: boolean;
  branch?: string;
  url?: string;
  actor?: string;
  updatedAt?: string;
  review?: GithubPRReview;
};

export type GithubIssue = {
  number: number;
  title: string;
  state: string;
  url?: string;
  actor?: string;
  /** The provider's own label set (MirrorPayload.Labels), projected verbatim.
   * The Issues Kanban board derives its priority badge from these — absent
   * means "no label", never a fabricated one. */
  labels?: string[];
  /** The provider's own assignees (MirrorPayload.Assignees), projected verbatim.
   * Absent means "unassigned", never a fabricated agent. */
  assignees?: string[];
  /** ISI-4760 (Epic 4) — the OPTIONAL local-bridge block, mirroring the Epic 2
   * read contract (ISI-4757 §7) EXACTLY. Present only when this GitHub issue is
   * bridged to a Paperclip work-item (label `ksquad.github.issue=<owner>/<repo>#N`);
   * ABSENT means "not locally assigned" — the honest default, never fabricated.
   * It is a LOCAL-ONLY status derived from the work-item/run store; it is NEVER
   * a GitHub assignee and must never merge into `assignees` (ADR-0013 §D4 honesty
   * guard / contract §6). Sourced inside the same githubstatus.go mirror read, so
   * it rides the existing BFF payload — no extra per-card request. */
  local?: {
    workItemId: string;
    agent: string;
    runState: "running" | "todo" | "done";
  };
  updatedAt?: string;
};

export type GithubCheck = {
  name: string;
  state: string;
  conclusion?: string;
  url?: string;
};

export type GithubArtifact = {
  name: string;
  url?: string;
  sizeBytes?: number;
  createdAt?: string;
  expiresAt?: string;
};

export type GithubRelease = {
  name: string;
  tag?: string;
  state: string;
  url?: string;
  actor?: string;
  publishedAt?: string;
};

export type GithubBranch = {
  name: string;
  default?: boolean;
  headSha?: string;
  url?: string;
};

export type GithubFreshness = {
  lastMirrorTime?: string;
  lastWebhookTime?: string;
  mirrorRecordCount: number;
  /** DurableLastSyncedAt (ISI-5483) — the scm.repo anchor's last successful
   * mirror pass recorded on DISK, independent of the live CR status. Present
   * even when lastMirrorTime is absent (operator down / CR aged out). */
  durableLastSyncedAt?: string;
};

/** GithubSync mirrors internal/apiserver/githubstatus.go GithubSync (ISI-4398 /
 * obs GH-4). It carries the three read-model fields the data-driven tab is keyed
 * on — reason (the SyncReady taxonomy), trigger (webhook|poll, proves the AC
 * "no manual kick"), and ageSeconds (the freshness SLI). All are derived by the
 * apiserver from Project.status; never a fabricated "live" signal. */
export type GithubSync = {
  reason: string;
  trigger?: string;
  ageSeconds?: number;
  /** Durable staleness fields (ISI-5483) read from the scm.repo mirror anchor,
   * NOT the live CR condition — they render cache age/health even when the
   * operator is down or the CR has aged out of the informer cache. `health` is
   * the durable class (RepoHealth), already DERIVED to "stale" by the apiserver
   * when the durable age outran the TTL. Absent when no durable anchor exists. */
  health?: string;
  durableAgeSeconds?: number;
  durableTtlSeconds?: number;
};

/** The durable scm.repo sync-health classes (pkg/scm RepoHealth*, surfaced 1:1
 * on the wire via GithubSync.health). "stale" is derived by the apiserver from
 * the anchor's age vs its TTL; the others are stamped by the reconciler. */
export const RepoHealth = {
  Healthy: "healthy",
  Degraded: "degraded",
  Stale: "stale",
  Error: "error",
  Unknown: "unknown",
} as const;

/** durableHealthLabel renders the DURABLE mirror staleness line (ISI-5483) from
 * the scm.repo anchor — the one freshness surface that survives the operator
 * being down. Returns null when no durable anchor exists (nothing to show).
 * Example: "Mirror on disk: healthy · synced 2 min ago · refresh every 5 min". */
export function durableHealthLabel(
  sync: GithubSync | undefined,
  freshness: GithubFreshness | undefined,
  now: number,
): { tone: ChipTone; text: string } | null {
  const health = sync?.health;
  if (!health) return null;

  // "synced N ago" from the durable age (preferred) or the durable timestamp.
  let ago = "";
  if (sync?.durableAgeSeconds !== undefined) {
    ago = `synced ${ageLabel(sync.durableAgeSeconds)}`;
  } else if (freshness?.durableLastSyncedAt) {
    ago = syncedAgo(freshness.durableLastSyncedAt, now);
  }

  // "refresh every N" from the TTL — drop the " ago" suffix ageLabel adds.
  const cadence =
    sync?.durableTtlSeconds && sync.durableTtlSeconds > 0
      ? ` · refresh every ${ageLabel(sync.durableTtlSeconds).replace(/ ago$/, "")}`
      : "";

  const tone: ChipTone =
    health === RepoHealth.Stale || health === RepoHealth.Degraded
      ? "paused"
      : health === RepoHealth.Error
        ? "blocked"
        : health === RepoHealth.Healthy
          ? "running"
          : "neutral";

  const label = health.charAt(0).toUpperCase() + health.slice(1);
  return { tone, text: `Mirror on disk: ${label}${ago ? ` · ${ago}` : ""}${cadence}` };
}

/** The reposync SyncReady reason taxonomy (repo_sync.go:70-76), surfaced 1:1 on
 * the wire. The tab keys its state cards + chip tone on these. */
export const SyncReason = {
  Synced: "Synced",
  NotConfigured: "SyncNotConfigured",
  CredentialMissing: "CredentialMissing",
  ProviderError: "ProviderError",
  MirrorWriteError: "MirrorWriteError",
  IssueSyncError: "IssueSyncError",
} as const;

/** Freshness SLO for the mirror age, in seconds: pollInterval (300s default,
 * repo_sync.go DefaultPollIntervalSeconds) + 60s slack — obs spec SLO #1
 * (p99 < pollInterval + slack). A Synced mirror older than this drives the
 * amber "Data is behind schedule" state. */
export const MIRROR_SLO_SECONDS = 360;

/** Chip tone maps 1:1 onto the locked status tones (globals.css): running
 * (green) · paused (amber) · blocked (red) · neutral (no chip). */
export type ChipTone = "running" | "paused" | "blocked" | "neutral";

export type ChipState = { tone: ChipTone; text: string };

/** chipState is the green→amber→red freshness-chip machine (DESIGN-SPEC §2),
 * keyed on sync.reason + mirror age. A Synced-but-stale mirror goes amber; a
 * provider error goes amber ("last good"); a missing credential goes red; an
 * unconfigured project shows no chip (neutral) — its empty state carries the CTA. */
export function chipState(sync: GithubSync | undefined): ChipState {
  const reason = sync?.reason ?? SyncReason.NotConfigured;
  const age = sync?.ageSeconds;
  const ago = age === undefined ? "" : ageLabel(age);

  switch (reason) {
    case SyncReason.NotConfigured:
      return { tone: "neutral", text: "" };
    case SyncReason.CredentialMissing:
      return { tone: "blocked", text: "Paused · reconnect required" };
    case SyncReason.ProviderError:
    case SyncReason.MirrorWriteError:
    case SyncReason.IssueSyncError:
      return { tone: "paused", text: ago ? `Retrying · last good ${ago}` : "Retrying" };
    case SyncReason.Synced:
    default:
      if (age !== undefined && age >= MIRROR_SLO_SECONDS) {
        // Synced but behind the freshness SLO — amber, and the "Sync delayed"
        // card explains it. Trigger source is dropped once we're stale.
        return { tone: "paused", text: ago ? `Synced · ${ago}` : "Synced" };
      }
      return {
        tone: "running",
        text:
          `Synced${ago ? ` · ${ago}` : ""}` +
          (sync?.trigger ? ` · via ${sync.trigger}` : ""),
      };
  }
}

/** ageLabel humanizes an age in seconds as "N sec/min/hr/days ago" — the chip +
 * mirror-age meter copy (DESIGN-SPEC uses "2 min ago", "14 min ago"). */
export function ageLabel(seconds: number): string {
  const s = Math.max(0, Math.round(seconds));
  if (s < 60) return `${s} sec ago`;
  const m = Math.round(s / 60);
  if (m < 60) return `${m} min ago`;
  const h = Math.round(m / 60);
  if (h < 24) return `${h} hr ago`;
  return `${Math.round(h / 24)} days ago`;
}

/** GET /api/projects/{id}/github response (apiserver githubstatus.go). Every
 * panel arrives as a non-null array (the Go read model seeds `[]`), so the tab
 * never crashes on a null slice. */
export type GithubStatus = {
  project: { name: string; namespace: string };
  pullRequests: GithubPR[];
  issues: GithubIssue[];
  checkRuns: GithubCheck[];
  artifacts: GithubArtifact[];
  releases: GithubRelease[];
  branches: GithubBranch[];
  freshness: GithubFreshness;
  sync: GithubSync;
};

/** The distinct honest state an HTTP status carries (mirrors SquadOverview). */
export type GithubStatusState =
  | { kind: "loading" }
  | { kind: "unauthenticated" }
  | { kind: "not-found" }
  | { kind: "not-wired" }
  | { kind: "error"; status: number }
  | { kind: "ready"; data: GithubStatus };

/** Map an HTTP status to the state it carries. 501 ⇒ the read model is not
 * wired in this deployment (S5b pending) — the tab renders "not available yet",
 * never fabricated rows. */
export function classifyGithubStatus(status: number): GithubStatusState {
  switch (status) {
    case 401:
      return { kind: "unauthenticated" };
    case 404:
      return { kind: "not-found" };
    case 501:
      return { kind: "not-wired" };
    default:
      return { kind: "error", status };
  }
}

/** POST to the "Sync now" BFF endpoint (ISI-4011). Returns the HTTP status so
 * the caller can handle 202 (triggered), 429 (debounced), or errors. */
export async function triggerGithubSync(projectId: string): Promise<number> {
  const res = await fetch(
    `/api/projects/${encodeURIComponent(projectId)}/github/sync`,
    { method: "POST", cache: "no-store" },
  );
  return res.status;
}

/** Fetch the GitHub-status payload through the BFF choke point. Returns the
 * classified state directly (200 ⇒ ready; else the honest state) so the caller
 * never fabricates rows. The BFF relays the apiserver status verbatim. */
export async function fetchGithubStatus(
  projectId: string,
): Promise<GithubStatusState> {
  const res = await fetch(
    `/api/projects/${encodeURIComponent(projectId)}/github`,
    { cache: "no-store" },
  );
  if (res.ok) {
    return { kind: "ready", data: (await res.json()) as GithubStatus };
  }
  return classifyGithubStatus(res.status);
}

/** Humanize a mirror timestamp as "synced Ns ago" relative to `now` (ms). Nil
 * ⇒ "not synced yet" — the honest absence, never a fake "live". */
export function syncedAgo(iso: string | undefined, now: number): string {
  if (!iso) return "not synced yet";
  const then = Date.parse(iso);
  if (Number.isNaN(then)) return "not synced yet";
  const secs = Math.max(0, Math.round((now - then) / 1000));
  if (secs < 60) return `synced ${secs}s ago`;
  const mins = Math.round(secs / 60);
  if (mins < 60) return `synced ${mins}m ago`;
  const hrs = Math.round(mins / 60);
  if (hrs < 24) return `synced ${hrs}h ago`;
  return `synced ${Math.round(hrs / 24)}d ago`;
}

// ============================================================================
// Review automation config (ISI-4764 / ISI-4750 E2) — the client contract for
// the E1 sub-resource BFF route (app/api/projects/[id]/repo/review-automation).
//
// Types mirror the E1 apiserver ReviewAutomationView (internal/apiserver/
// reviewautomation.go, PR #561) EXACTLY; the Go struct is the contract owner.
// Enum wire values are underscore-cased. `enabledBy` and `canEdit` are
// server-computed and READ-ONLY: `enabledBy` is the D1 provenance stamp (never
// sent back on write); `canEdit` is the caller's contributor write-tier, which
// gates the form here — E2 does NOT call fetchViewerRole() (ISI-4496 trap).
// ============================================================================

/** Which PRs the automation reviews. Default is `team_authored`. */
export type ReviewScope = "team_authored" | "all";
/** When a review fires. Default is `on_new_commits`. */
export type ReviewTrigger = "on_open" | "on_new_commits";

/** The GET read model + the 200 write-response body (reviewautomation.go
 * ReviewAutomationView). Enum fields always carry a resolved default. */
export type ReviewAutomationView = {
  enabled: boolean;
  reviewerAgentId: string;
  scope: ReviewScope;
  trigger: ReviewTrigger;
  /** Server-stamped D1 provenance (who last enabled it) — read-only. */
  enabledBy: string;
  /** Server-computed contributor write-tier — gates the form. */
  canEdit: boolean;
};

/** The write input the apiserver accepts — a strict subset of the view. It
 * structurally OMITS `enabledBy` (server-stamped, never body-trusted) and
 * `canEdit` (server-computed authZ). */
export type ReviewAutomationInput = {
  enabled: boolean;
  reviewerAgentId: string;
  scope: ReviewScope;
  trigger: ReviewTrigger;
};

/** Human labels for the enum wire values (dialog copy). */
export const REVIEW_SCOPE_LABEL: Record<ReviewScope, string> = {
  team_authored: "Team-authored PRs only",
  all: "All pull requests",
};
export const REVIEW_TRIGGER_LABEL: Record<ReviewTrigger, string> = {
  on_open: "When a pull request is opened",
  on_new_commits: "When new commits are pushed",
};

/** The distinct honest state the GET carries (mirrors GithubStatusState). */
export type ReviewAutomationState =
  | { kind: "loading" }
  | { kind: "ready"; view: ReviewAutomationView }
  | { kind: "unauthenticated" }
  | { kind: "not-found" }
  | { kind: "not-wired" }
  | { kind: "error"; status: number };

/** Fetch the review-automation config through the BFF choke point. Relays the
 * apiserver status verbatim: 501 ⇒ the service is not wired in this deployment
 * (the dialog renders its honest "not available yet" state), 404 ⇒ existence-
 * hiding, never fabricated config. */
export async function fetchReviewAutomation(
  projectId: string,
): Promise<ReviewAutomationState> {
  const res = await fetch(
    `/api/projects/${encodeURIComponent(projectId)}/repo/review-automation`,
    { cache: "no-store" },
  );
  if (res.ok) {
    return { kind: "ready", view: (await res.json()) as ReviewAutomationView };
  }
  switch (res.status) {
    case 401:
      return { kind: "unauthenticated" };
    case 404:
      return { kind: "not-found" };
    case 501:
      return { kind: "not-wired" };
    default:
      return { kind: "error", status: res.status };
  }
}

/** The outcome of a review-automation write (PUT). Distinguishes the field-
 * level rejections the dialog surfaces inline (400 bad body / 422 invalid enum
 * or ineligible reviewer) from the deploy-level unavailability (501/502) and
 * the authZ deny (403). */
export type ReviewAutomationSaveResult =
  | { kind: "saved"; view: ReviewAutomationView }
  | { kind: "invalid"; message: string; fields: string[] }
  | { kind: "denied" }
  | { kind: "unavailable"; status: number }
  | { kind: "error"; status: number };

/** Read the apiserver's `{error, fields}` rejection body (best-effort — a
 * missing/garbled body degrades to a generic message, never a throw). */
async function readRejection(
  res: Response,
): Promise<{ message: string; fields: string[] }> {
  try {
    const body = (await res.json()) as { error?: unknown; fields?: unknown };
    const message = typeof body.error === "string" && body.error ? body.error : "";
    const fields = Array.isArray(body.fields)
      ? body.fields.filter((f): f is string => typeof f === "string")
      : [];
    return { message, fields };
  } catch {
    return { message: "", fields: [] };
  }
}

/** PUT the review-automation config through the BFF. The apiserver owns the
 * authoritative validation (deny-by-default authZ, D5 reviewer eligibility), so
 * this never pre-validates — it just classifies the relayed status so the
 * dialog can surface a 422 inline against the offending fields. */
export async function saveReviewAutomation(
  projectId: string,
  input: ReviewAutomationInput,
): Promise<ReviewAutomationSaveResult> {
  const res = await fetch(
    `/api/projects/${encodeURIComponent(projectId)}/repo/review-automation`,
    {
      method: "PUT",
      headers: { "content-type": "application/json" },
      body: JSON.stringify(input),
      cache: "no-store",
    },
  );
  if (res.ok) {
    return { kind: "saved", view: (await res.json()) as ReviewAutomationView };
  }
  switch (res.status) {
    case 400:
    case 422: {
      const { message, fields } = await readRejection(res);
      return {
        kind: "invalid",
        message:
          message ||
          "The apiserver rejected this configuration — check the reviewer and options.",
        fields,
      };
    }
    case 403:
      return { kind: "denied" };
    case 501:
    case 502:
      return { kind: "unavailable", status: res.status };
    default:
      return { kind: "error", status: res.status };
  }
}

// ============================================================================
// Issue auto-triage config (ISI-5595 WS-E) — the client contract for the
// spec.repo.automation.issueTriage BFF route
// (app/api/projects/[id]/repo/issue-triage).
//
// Types mirror the apiserver IssueTriageView (internal/apiserver/issuetriage.go)
// EXACTLY; the Go struct is the contract owner. `enabledBy` (provenance) and
// `canEdit` (contributor write-tier) are server-computed and READ-ONLY — the
// dialog gates the form on `canEdit` without a speculative PUT, exactly like the
// review-automation dialog. Shaped as a near-clone of ReviewAutomation* so the
// three automation dialogs read/write uniformly.
// ============================================================================

/** The GET read model + the 200 write-response body (issuetriage.go
 * IssueTriageView). `labelFilter` is always an array; `onlyUnassigned` carries its
 * resolved default (true). */
export type IssueTriageView = {
  enabled: boolean;
  triageAgentId: string;
  labelFilter: string[];
  onlyUnassigned: boolean;
  /** Server-stamped provenance (who last enabled it) — read-only. */
  enabledBy: string;
  /** Server-computed contributor write-tier — gates the form. */
  canEdit: boolean;
};

/** The write input the apiserver accepts — a strict subset of the view. It
 * structurally OMITS `enabledBy`/`enabledAt` (server-stamped) and `canEdit`. */
export type IssueTriageInput = {
  enabled: boolean;
  triageAgentId: string;
  labelFilter: string[];
  onlyUnassigned: boolean;
};

export type IssueTriageState =
  | { kind: "loading" }
  | { kind: "ready"; view: IssueTriageView }
  | { kind: "unauthenticated" }
  | { kind: "not-found" }
  | { kind: "not-wired" }
  | { kind: "error"; status: number };

/** Fetch the issue-triage config through the BFF choke point. Relays the apiserver
 * status verbatim (501 ⇒ not wired, 404 ⇒ existence-hiding), never fabricated. */
export async function fetchIssueTriage(
  projectId: string,
): Promise<IssueTriageState> {
  const res = await fetch(
    `/api/projects/${encodeURIComponent(projectId)}/repo/issue-triage`,
    { cache: "no-store" },
  );
  if (res.ok) {
    return { kind: "ready", view: (await res.json()) as IssueTriageView };
  }
  switch (res.status) {
    case 401:
      return { kind: "unauthenticated" };
    case 404:
      return { kind: "not-found" };
    case 501:
      return { kind: "not-wired" };
    default:
      return { kind: "error", status: res.status };
  }
}

export type IssueTriageSaveResult =
  | { kind: "saved"; view: IssueTriageView }
  | { kind: "invalid"; message: string; fields: string[] }
  | { kind: "denied" }
  | { kind: "unavailable"; status: number }
  | { kind: "error"; status: number };

/** PUT the issue-triage config through the BFF. The apiserver owns the
 * authoritative validation (deny-by-default authZ, enabled⇒triageAgentId), so this
 * never pre-validates — it classifies the relayed status so the dialog can surface
 * a 422 inline against the offending fields. */
export async function saveIssueTriage(
  projectId: string,
  input: IssueTriageInput,
): Promise<IssueTriageSaveResult> {
  const res = await fetch(
    `/api/projects/${encodeURIComponent(projectId)}/repo/issue-triage`,
    {
      method: "PUT",
      headers: { "content-type": "application/json" },
      body: JSON.stringify(input),
      cache: "no-store",
    },
  );
  if (res.ok) {
    return { kind: "saved", view: (await res.json()) as IssueTriageView };
  }
  switch (res.status) {
    case 400:
    case 422: {
      const { message, fields } = await readRejection(res);
      return {
        kind: "invalid",
        message:
          message ||
          "The apiserver rejected this configuration — check the triage agent and options.",
        fields,
      };
    }
    case 403:
      return { kind: "denied" };
    case 501:
    case 502:
      return { kind: "unavailable", status: res.status };
    default:
      return { kind: "error", status: res.status };
  }
}

// ============================================================================
// CI-failure triage config (ISI-5595 WS-E) — the client contract for the
// spec.repo.automation.ciFailure BFF route
// (app/api/projects/[id]/repo/ci-automation).
//
// Types mirror the apiserver CiFailureView (internal/apiserver/cifailure.go)
// EXACTLY; the Go struct is the contract owner. Same read-only provenance/canEdit
// discipline as the other two automation dialogs.
// ============================================================================

/** Which terminal check-run conclusions qualify as a failure. */
export type CiFailureConclusion = "failure" | "timed_out" | "cancelled";

/** The ordered conclusion vocabulary + human labels for the selector. */
export const CI_FAILURE_CONCLUSIONS: CiFailureConclusion[] = [
  "failure",
  "timed_out",
  "cancelled",
];
export const CI_FAILURE_CONCLUSION_LABEL: Record<CiFailureConclusion, string> = {
  failure: "Failed",
  timed_out: "Timed out",
  cancelled: "Cancelled",
};

/** The GET read model + the 200 write-response body (cifailure.go CiFailureView).
 * `branchFilter` is always an array; `conclusions` carries its resolved default
 * (["failure"]). */
export type CiFailureView = {
  enabled: boolean;
  agentId: string;
  branchFilter: string[];
  conclusions: CiFailureConclusion[];
  /** Server-stamped provenance — read-only. */
  enabledBy: string;
  /** Server-computed contributor write-tier — gates the form. */
  canEdit: boolean;
};

/** The write input — OMITS `enabledBy`/`enabledAt` (server-stamped) + `canEdit`. */
export type CiFailureInput = {
  enabled: boolean;
  agentId: string;
  branchFilter: string[];
  conclusions: CiFailureConclusion[];
};

export type CiFailureState =
  | { kind: "loading" }
  | { kind: "ready"; view: CiFailureView }
  | { kind: "unauthenticated" }
  | { kind: "not-found" }
  | { kind: "not-wired" }
  | { kind: "error"; status: number };

/** Fetch the CI-failure config through the BFF choke point (status relayed
 * verbatim). */
export async function fetchCiFailure(
  projectId: string,
): Promise<CiFailureState> {
  const res = await fetch(
    `/api/projects/${encodeURIComponent(projectId)}/repo/ci-automation`,
    { cache: "no-store" },
  );
  if (res.ok) {
    return { kind: "ready", view: (await res.json()) as CiFailureView };
  }
  switch (res.status) {
    case 401:
      return { kind: "unauthenticated" };
    case 404:
      return { kind: "not-found" };
    case 501:
      return { kind: "not-wired" };
    default:
      return { kind: "error", status: res.status };
  }
}

export type CiFailureSaveResult =
  | { kind: "saved"; view: CiFailureView }
  | { kind: "invalid"; message: string; fields: string[] }
  | { kind: "denied" }
  | { kind: "unavailable"; status: number }
  | { kind: "error"; status: number };

/** PUT the CI-failure config through the BFF. The apiserver owns validation
 * (conclusions enum, enabled⇒agentId); this just classifies the relayed status. */
export async function saveCiFailure(
  projectId: string,
  input: CiFailureInput,
): Promise<CiFailureSaveResult> {
  const res = await fetch(
    `/api/projects/${encodeURIComponent(projectId)}/repo/ci-automation`,
    {
      method: "PUT",
      headers: { "content-type": "application/json" },
      body: JSON.stringify(input),
      cache: "no-store",
    },
  );
  if (res.ok) {
    return { kind: "saved", view: (await res.json()) as CiFailureView };
  }
  switch (res.status) {
    case 400:
    case 422: {
      const { message, fields } = await readRejection(res);
      return {
        kind: "invalid",
        message:
          message ||
          "The apiserver rejected this configuration — check the triage agent and options.",
        fields,
      };
    }
    case 403:
      return { kind: "denied" };
    case 501:
    case 502:
      return { kind: "unavailable", status: res.status };
    default:
      return { kind: "error", status: res.status };
  }
}

/** The mirror is "stale" when its last mirror time is older than `thresholdMs`
 * (default 10 min) — the tab shows a subdued "may be stale" hint (never blocks). */
export function isStale(
  freshness: GithubFreshness | undefined,
  now: number,
  thresholdMs = 10 * 60 * 1000,
): boolean {
  const iso = freshness?.lastMirrorTime;
  if (!iso) return false; // "not synced yet" is its own state, not "stale".
  const then = Date.parse(iso);
  if (Number.isNaN(then)) return false;
  return now - then > thresholdMs;
}
