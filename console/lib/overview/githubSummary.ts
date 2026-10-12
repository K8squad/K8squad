// lib/overview/githubSummary.ts — client seam + pure projections for the fleet
// GitHub summary strip (ISI-5705 WS-1b; GET /api/squad/github), consumed by the
// ISI-5708 WS-3 fleet recompose. Mirrors the apiserver GithubSummary wire model
// (githubsummary.go) — keep in lockstep.
//
// The apiserver aggregates the SYNCED scm mirror server-side (no outbound GitHub
// call, no credential), so this client is a thin fetch + honest degrade. Fabrication
// discipline (FR-I3, ISI-4229): an unavailable source degrades to `not-available`
// (the strip renders an empty state), an authoritative zero renders `0`.

/** Fleet headline counts (every field a mirror-derived count). */
export interface GithubTotals {
  issuesAssigned: number;
  issuesOpen: number;
  prsOpened: number;
  prsReviewed: number;
  prsMerged: number;
  checksRed: number;
}

/** One repository's slice of the totals, keyed by the lower-cased owner/repo slug. */
export interface GithubRepoStat {
  repo: string;
  assigned: number;
  open: number;
  /** Authoritative OPEN PR count — the per-project "Open PRs" that used to render "—". */
  prs: number;
  reviewed: number;
  checksRed: number;
}

/** The GET /api/squad/github payload. */
export interface GithubSummary {
  totals: GithubTotals;
  /** 7-day merge trend, oldest→newest, length 7. */
  mergeSparkline7d: number[];
  byRepo: GithubRepoStat[];
}

export type GithubSummaryState =
  | { kind: "loading" }
  | { kind: "not-available"; status: number }
  | { kind: "error"; status: number }
  | { kind: "ready"; data: GithubSummary };

const ZERO_TOTALS: GithubTotals = {
  issuesAssigned: 0,
  issuesOpen: 0,
  prsOpened: 0,
  prsReviewed: 0,
  prsMerged: 0,
  checksRed: 0,
};

/** 404/501 degrade (mirror read model unwired on this deployment); else retryable error. */
export function classifyGithubStatus(
  status: number,
): Extract<GithubSummaryState, { kind: "not-available" | "error" }> {
  return status === 404 || status === 501
    ? { kind: "not-available", status }
    : { kind: "error", status };
}

export async function fetchGithubSummary(signal?: AbortSignal): Promise<GithubSummaryState> {
  try {
    const res = await fetch("/api/squad/github", { headers: { accept: "application/json" }, signal });
    if (!res.ok) return classifyGithubStatus(res.status);
    const body = (await res.json()) as Partial<GithubSummary>;
    return {
      kind: "ready",
      data: {
        totals: body.totals ?? ZERO_TOTALS,
        mergeSparkline7d: body.mergeSparkline7d ?? [],
        byRepo: body.byRepo ?? [],
      },
    };
  } catch {
    return { kind: "error", status: 0 };
  }
}

// ---------------------------------------------------------------------------
// Pure projections (exported for unit tests — no fetch, no DOM).
// ---------------------------------------------------------------------------

/**
 * Normalize a project's repo URL to the mirror's lower-cased `owner/repo` slug so
 * the per-project jump row can look up its authoritative open-PR count in `byRepo`.
 * Handles `https://github.com/owner/repo(.git)`, `git@github.com:owner/repo.git`,
 * and bare `owner/repo`. Returns null when no slug can be derived (the row then
 * renders "—" open PRs rather than an invented 0 — FR-I3).
 */
export function repoSlug(repoUrl: string | null | undefined): string | null {
  const raw = (repoUrl ?? "").trim();
  if (!raw) return null;
  // Strip scheme/host and any scp-like `git@host:` prefix, keep the path tail.
  let path = raw
    .replace(/^[a-z]+:\/\/[^/]+\//i, "") // https://host/
    .replace(/^git@[^:]+:/i, "") // git@host:
    .replace(/\.git$/i, "")
    .replace(/^\/+|\/+$/g, "");
  // If a host slipped through (bare "github.com/owner/repo"), drop the leading host.
  const parts = path.split("/").filter(Boolean);
  if (parts.length > 2 && parts[0].includes(".")) parts.shift();
  if (parts.length < 2) return null;
  return `${parts[parts.length - 2]}/${parts[parts.length - 1]}`.toLowerCase();
}

/** Map each repo slug → its open-PR count, for the per-project jump row lookup. */
export function openPrsByRepo(summary: GithubSummary | null | undefined): Map<string, number> {
  const m = new Map<string, number>();
  for (const r of summary?.byRepo ?? []) m.set(r.repo.toLowerCase(), r.prs);
  return m;
}
