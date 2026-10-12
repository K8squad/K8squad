// lib/overview/githubSummary.ts — the FE contract + pure projections behind the overview
// GitHub strip and the real per-project "Open PRs" count (ISI-5705 WS-1b, parent plan
// ISI-5697 rev2, approved design ISI-5699 mock D).
//
// These types mirror the WS-1b wire shape (internal/apiserver/githubsummary.go GithubSummary):
// ONE fleet-wide roll-up sourced entirely from the already-synced scm mirror — never a live
// GitHub fan-out — with a per-repo breakdown. The project overview folds that fleet summary down
// to THIS project's repo row (keyed by the lower-cased owner/repo slug derived from the project's
// repoUrl), so the "Open PRs" tile and the GitHub strip render authoritative counts instead of
// the hard-coded "—" they showed before.
//
// Fabrication discipline (FR-I3, ISI-4229 degrade-don't-blank): when the summary source is
// unavailable, or when this project's repo is not in the mirror, every count degrades to "—"
// (UNAVAILABLE) rather than an invented 0. Only an authoritative mirror row renders a real 0.

/** Fleet headline totals (githubsummary.go GithubTotals). */
export interface GithubTotals {
  issuesAssigned: number;
  issuesOpen: number;
  prsOpened: number;
  prsReviewed: number;
  prsMerged: number;
  checksRed: number;
}

/** One repository's slice of the fleet totals (githubsummary.go GithubRepoStat). `repo` is the
 * lower-cased owner/repo slug; `prs` is the authoritative OPEN-PR count the project tile renders. */
export interface GithubRepoStat {
  repo: string;
  assigned: number;
  open: number;
  prs: number;
  reviewed: number;
  checksRed: number;
}

/** GET /api/squad/github payload (githubsummary.go GithubSummary). */
export interface GithubSummary {
  totals: GithubTotals;
  mergeSparkline7d: number[];
  byRepo: GithubRepoStat[];
}

export const UNAVAILABLE = "—";

/**
 * Normalize a project's repoUrl to the lower-cased `owner/repo` slug the WS-1b byRepo rows are
 * keyed by. Accepts the shapes the mirror produces — `https://github.com/Owner/Repo`,
 * a trailing `.git`, an `git@github.com:Owner/Repo` scp-form, a trailing slash — and returns null
 * for anything it cannot confidently reduce to exactly two path segments (so an unparseable URL
 * degrades the tile to "—" rather than matching the wrong repo).
 */
export function repoSlug(repoUrl: string | null | undefined): string | null {
  if (!repoUrl) return null;
  let rest = repoUrl.trim();
  if (!rest) return null;
  // scp-form: git@host:owner/repo(.git)
  const scp = rest.match(/^[^@]+@[^:]+:(.+)$/);
  if (scp) {
    rest = scp[1];
  } else {
    // strip scheme + host from a URL form; leave a bare "owner/repo" untouched.
    rest = rest.replace(/^[a-z]+:\/\//i, "");
    const slash = rest.indexOf("/");
    if (slash >= 0 && rest.slice(0, slash).includes(".")) {
      // first segment looks like a host (has a dot) → drop it.
      rest = rest.slice(slash + 1);
    }
  }
  rest = rest.replace(/\.git$/i, "").replace(/^\/+|\/+$/g, "");
  const parts = rest.split("/").filter(Boolean);
  if (parts.length !== 2) return null;
  return `${parts[0]}/${parts[1]}`.toLowerCase();
}

/**
 * Select THIS project's per-repo stat from the fleet summary by matching the project's repoUrl
 * slug against the byRepo rows (both lower-cased). Returns null when the summary has no row for
 * the repo (unmirrored / unparseable url) so the caller degrades to "—" (FR-I3) instead of
 * fabricating a zero.
 */
export function selectRepoStat(
  summary: GithubSummary | null | undefined,
  repoUrl: string | null | undefined,
): GithubRepoStat | null {
  const slug = repoSlug(repoUrl);
  if (!slug || !summary?.byRepo) return null;
  return summary.byRepo.find((r) => r.repo?.toLowerCase() === slug) ?? null;
}

/** A count rendered FR-I3-honestly: a number when the stat row exists, "—" when it does not. */
export function repoCount(stat: GithubRepoStat | null, field: keyof GithubRepoStat): number | string {
  if (!stat) return UNAVAILABLE;
  const v = stat[field];
  return typeof v === "number" ? v : UNAVAILABLE;
}
