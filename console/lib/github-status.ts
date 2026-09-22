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
};

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
