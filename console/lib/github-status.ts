// lib/github-status.ts — ISI-3956 S5c: the GitHub-status payload types + the
// browser-side fetcher for the BFF route.
//
// Types mirror internal/apiserver/githubstatus.go EXACTLY; the Go structs are
// the contract owner. The tab reads the scm MIRROR (never GitHub), so freshness
// is honest — the response carries the mirror's own timestamps, which the tab
// renders as "synced Ns ago", NEVER a fabricated "live" badge (ADR-0013 §D4).
// The BYO repo credential is never present (the apiserver never returns it).

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
};

export type GithubIssue = {
  number: number;
  title: string;
  state: string;
  url?: string;
  actor?: string;
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
