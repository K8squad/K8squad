"use client";

// components/GitHubStatusTab.tsx — ISI-3956 S5c: the Project GitHub-status tab.
//
// Renders the scm-mirror projection (PRs / issues / check-runs / artifacts /
// releases / branches) the S5b read model returns, with HONEST freshness: "synced Ns ago"
// from the mirror's own timestamps (ADR-0013 §D4), never a fabricated "live"
// badge. A short (~30s) client-side interval re-fetches the CACHED read model
// (S5b) — it never reaches GitHub, so freshness updates without burning rate
// budget. Every terminal HTTP state the BFF relays gets a distinct honest
// rendering: 401 unauthenticated · 404 existence-hiding · 501 not-wired · empty
// mirror · retryable error — never fabricated rows.
//
// "Sync now" (bumping the scm-sync-trigger annotation) is intentionally NOT here:
// it needs an apiserver write endpoint that does not exist yet — tracked as a
// follow-up. The mirror + auto-refresh + honest freshness is the whole read tab.

import { useCallback, useEffect, useState, type ReactNode } from "react";
import { EmptyState } from "@/components/forms/EmptyState";
import {
  fetchGithubStatus,
  isStale,
  syncedAgo,
  type GithubStatus,
  type GithubStatusState,
} from "@/lib/github-status";

const AUTO_REFRESH_MS = 30_000;

export function GitHubStatusTab({ projectId }: { projectId: string }) {
  const [state, setState] = useState<GithubStatusState>({ kind: "loading" });
  // `now` drives the "synced Ns ago" label; bumped on each refresh tick.
  const [now, setNow] = useState<number>(() => Date.now());

  const load = useCallback(async () => {
    try {
      const next = await fetchGithubStatus(projectId);
      setState(next);
    } catch {
      // A network throw (BFF unreachable) is a retryable transport error.
      setState({ kind: "error", status: 0 });
    }
    setNow(Date.now());
  }, [projectId]);

  useEffect(() => {
    void load();
    const id = setInterval(() => {
      void load();
    }, AUTO_REFRESH_MS);
    return () => clearInterval(id);
  }, [load]);

  if (state.kind === "loading") {
    return (
      <section aria-busy="true" data-testid="github-loading">
        <h1>GitHub status</h1>
        <p className="muted">Loading repo status from the mirror…</p>
      </section>
    );
  }

  if (state.kind === "unauthenticated") {
    return honest("Not signed in", "Your session has expired — sign in to view GitHub status.");
  }
  if (state.kind === "not-found") {
    return honest("No GitHub status", "This project has no GitHub status, or you cannot access it.");
  }
  if (state.kind === "not-wired") {
    return honest(
      "GitHub status not available yet",
      "The GitHub-status read model is not wired in this deployment (the operator mirror is not exposed here yet).",
    );
  }
  if (state.kind === "error") {
    return honest(
      "Couldn't load GitHub status",
      `The mirror read failed${state.status ? ` (HTTP ${state.status})` : ""} — it will retry automatically.`,
    );
  }

  const data = state.data;
  const empty =
    data.pullRequests.length === 0 &&
    data.issues.length === 0 &&
    data.checkRuns.length === 0 &&
    data.artifacts.length === 0 &&
    data.releases.length === 0 &&
    data.branches.length === 0;

  const stale = isStale(data.freshness, now);

  return (
    <section data-testid="github-status">
      <header className="github-status__head">
        <h1>GitHub status</h1>
        <p className="muted" data-testid="github-freshness">
          {syncedAgo(data.freshness.lastMirrorTime, now)}
          {stale && (
            <span className="github-status__stale" data-testid="github-stale">
              {" "}· may be stale
            </span>
          )}
        </p>
      </header>

      {empty ? (
        <EmptyState
          testId="github-empty"
          title="No GitHub activity yet"
          why="The mirror has no PRs, issues, checks, artifacts, releases or branches for this project's repo yet."
        />
      ) : (
        <div className="github-status__panels">
          <PRPanel prs={data.pullRequests} />
          <IssuePanel issues={data.issues} />
          <CheckPanel checks={data.checkRuns} />
          <ArtifactPanel artifacts={data.artifacts} />
          <ReleasePanel releases={data.releases} />
          <BranchPanel branches={data.branches} />
        </div>
      )}
    </section>
  );
}

function honest(title: string, why: string) {
  return (
    <section data-testid="github-status">
      <h1>GitHub status</h1>
      <EmptyState testId="github-honest" title={title} why={why} />
    </section>
  );
}

/** A GitHub deep-link, or plain text when the normalized url is absent. */
function Link({ url, children }: { url?: string; children: ReactNode }) {
  if (!url) return <>{children}</>;
  return (
    <a href={url} target="_blank" rel="noreferrer noopener">
      {children}
    </a>
  );
}

function PRPanel({ prs }: { prs: GithubStatus["pullRequests"] }) {
  if (prs.length === 0) return null;
  return (
    <div className="card" data-testid="panel-prs">
      <h2>Pull requests</h2>
      <ul>
        {prs.map((pr) => (
          <li key={`pr-${pr.number}`} data-testid="pr-row">
            <Link url={pr.url}>#{pr.number} {pr.title}</Link>{" "}
            <span className="muted">
              {pr.reviewState || pr.state}
              {pr.branch ? ` · ${pr.branch}` : ""}
            </span>
          </li>
        ))}
      </ul>
    </div>
  );
}

function IssuePanel({ issues }: { issues: GithubStatus["issues"] }) {
  if (issues.length === 0) return null;
  return (
    <div className="card" data-testid="panel-issues">
      <h2>Issues</h2>
      <ul>
        {issues.map((it) => (
          <li key={`issue-${it.number}`} data-testid="issue-row">
            <Link url={it.url}>#{it.number} {it.title}</Link>{" "}
            <span className="muted">{it.state}</span>
          </li>
        ))}
      </ul>
    </div>
  );
}

function CheckPanel({ checks }: { checks: GithubStatus["checkRuns"] }) {
  if (checks.length === 0) return null;
  return (
    <div className="card" data-testid="panel-checks">
      <h2>Checks</h2>
      <ul>
        {checks.map((c, i) => (
          <li key={`check-${c.name}-${i}`} data-testid="check-row">
            <Link url={c.url}>{c.name}</Link>{" "}
            <span className="muted">{c.conclusion || c.state}</span>
          </li>
        ))}
      </ul>
    </div>
  );
}

function ArtifactPanel({ artifacts }: { artifacts: GithubStatus["artifacts"] }) {
  if (artifacts.length === 0) return null;
  return (
    <div className="card" data-testid="panel-artifacts">
      <h2>Artifacts</h2>
      <ul>
        {artifacts.map((a, i) => (
          <li key={`artifact-${a.name}-${i}`} data-testid="artifact-row">
            <Link url={a.url}>{a.name}</Link>
          </li>
        ))}
      </ul>
    </div>
  );
}

function ReleasePanel({ releases }: { releases: GithubStatus["releases"] }) {
  if (releases.length === 0) return null;
  return (
    <div className="card" data-testid="panel-releases">
      <h2>Releases</h2>
      <ul>
        {releases.map((r, i) => (
          <li key={`release-${r.tag || r.name}-${i}`} data-testid="release-row">
            <Link url={r.url}>{r.name}</Link>{" "}
            <span className="muted">
              {r.state}
              {r.tag ? ` · ${r.tag}` : ""}
            </span>
          </li>
        ))}
      </ul>
    </div>
  );
}

function BranchPanel({ branches }: { branches: GithubStatus["branches"] }) {
  if (branches.length === 0) return null;
  return (
    <div className="card" data-testid="panel-branches">
      <h2>Branches</h2>
      <ul>
        {branches.map((b) => (
          <li key={`branch-${b.name}`} data-testid="branch-row">
            <Link url={b.url}>{b.name}</Link>{" "}
            <span className="muted">
              {b.default ? "default" : ""}
              {b.headSha ? ` · ${b.headSha.slice(0, 7)}` : ""}
            </span>
          </li>
        ))}
      </ul>
    </div>
  );
}
