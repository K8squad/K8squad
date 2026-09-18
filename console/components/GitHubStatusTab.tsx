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
// "Sync now" (ISI-4011): POST /api/projects/{id}/github/sync bumps the
// scm-sync-trigger annotation through the BFF; the reconciler fires
// asynchronously. The button is debounced client-side (30 s) to match the
// server-side window — a 429 from the server also latches the client debounce.

import { useCallback, useEffect, useRef, useState, type ReactNode } from "react";
import { EmptyState } from "@/components/forms/EmptyState";
import { ReleasesBranchesScreen } from "@/components/github/ReleasesBranchesScreen";
import {
  chipState,
  fetchGithubStatus,
  triggerGithubSync,
  SyncReason,
  type ChipTone,
  type GithubStatus,
  type GithubStatusState,
  type GithubSync,
} from "@/lib/github-status";

const AUTO_REFRESH_MS = 30_000;
// Client-side debounce matches the server-side 30 s window.
const SYNC_DEBOUNCE_MS = 30_000;

export function GitHubStatusTab({ projectId }: { projectId: string }) {
  const [state, setState] = useState<GithubStatusState>({ kind: "loading" });
  const [syncing, setSyncing] = useState(false);
  // Which GitHub screen is shown inside this tab: "status" is the S5c sync-health
  // view, "releases" is the ISI-4675 Releases & Branches screen. Both read the
  // same mirror projection; the shared chip/banner stays above the view switch.
  const [view, setView] = useState<"status" | "releases">("status");
  // Unix ms of the last successful sync trigger; controls button disable.
  const lastSyncRef = useRef<number>(0);

  const load = useCallback(async () => {
    try {
      const next = await fetchGithubStatus(projectId);
      setState(next);
    } catch {
      // A network throw (BFF unreachable) is a retryable transport error.
      setState({ kind: "error", status: 0 });
    }
  }, [projectId]);

  useEffect(() => {
    void load();
    const id = setInterval(() => {
      void load();
    }, AUTO_REFRESH_MS);
    return () => clearInterval(id);
  }, [load]);

  const handleSync = useCallback(async () => {
    if (syncing || Date.now() - lastSyncRef.current < SYNC_DEBOUNCE_MS) return;
    setSyncing(true);
    try {
      const status = await triggerGithubSync(projectId);
      if (status === 202 || status === 429) {
        // 202 = triggered; 429 = server debounce (treat as triggered too).
        lastSyncRef.current = Date.now();
        // Short re-fetch after a moment to pick up the reconcile result.
        setTimeout(() => void load(), 3_000);
      }
    } catch {
      // ignore — button re-enables after debounce window
    } finally {
      setSyncing(false);
    }
  }, [projectId, syncing, load]);

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
  const sync: GithubSync = data.sync ?? { reason: SyncReason.NotConfigured };
  const chip = chipState(sync);
  const card = stateCardFor(sync);
  const empty =
    data.pullRequests.length === 0 &&
    data.issues.length === 0 &&
    data.checkRuns.length === 0 &&
    data.artifacts.length === 0 &&
    data.releases.length === 0 &&
    data.branches.length === 0;

  // Degrade, don't blank (DESIGN-SPEC §3): on a credential/provider/stale error
  // we keep the last-good mirror data visible but ghosted, alongside the banner
  // card that says WHY it's stale — the tab is never blank on a transient error.
  const ghost = card?.ghost === true && !empty;

  return (
    <section data-testid="github-status">
      <header className="github-status__head">
        <h1>GitHub status</h1>
        {chip.tone !== "neutral" && (
          <span
            className={`github-chip github-chip--${chip.tone}`}
            data-testid="github-chip"
            data-tone={chip.tone}
            data-reason={sync.reason}
          >
            <span className="github-chip__dot" aria-hidden="true" />
            {/* Kept lowercase-"synced …ago" phrase inside so existing honest-
               freshness assertions and screen-readers still parse the recency. */}
            <span data-testid="github-freshness">{chip.text}</span>
          </span>
        )}
        {/* Refresh is an escape hatch only — the AC is that refresh is automatic
           (webhook + poll), so this is deliberately understated, not a prominent
           "Sync now". A 429 latches the same client debounce. */}
        <button
          type="button"
          className="github-refresh"
          onClick={() => void handleSync()}
          disabled={syncing}
          data-testid="github-sync-now"
          aria-label="Refresh the mirror now"
          title="Refresh"
        >
          {syncing ? "Refreshing…" : "↻ Refresh"}
        </button>
      </header>

      {chip.tone === "running" && (
        <p className="muted github-status__autoline" data-testid="github-autoline">
          ⚡ Auto-refreshed — webhook + 5-min poll · no manual kick
        </p>
      )}

      {/* Global sync-health banner: stays above the view switch so a stale or
         errored mirror is visible on every GitHub screen, not just Sync status. */}
      {card && (
        <StateCard card={card} syncing={syncing} onRetry={() => void handleSync()} projectId={projectId} />
      )}

      <nav
        className="github-tabs"
        role="tablist"
        aria-label="GitHub views"
        data-testid="github-tabs"
      >
        <button
          type="button"
          role="tab"
          id="github-tab-status"
          aria-selected={view === "status"}
          aria-controls="github-view-status"
          className={`github-tab${view === "status" ? " github-tab--active" : ""}`}
          onClick={() => setView("status")}
          data-testid="github-tab-status"
        >
          Sync status
        </button>
        <button
          type="button"
          role="tab"
          id="github-tab-releases"
          aria-selected={view === "releases"}
          aria-controls="github-view-releases"
          className={`github-tab${view === "releases" ? " github-tab--active" : ""}`}
          onClick={() => setView("releases")}
          data-testid="github-tab-releases"
        >
          Releases &amp; Branches
        </button>
      </nav>

      {view === "releases" ? (
        <div
          id="github-view-releases"
          role="tabpanel"
          aria-labelledby="github-tab-releases"
          className="github-view"
        >
          <ReleasesBranchesScreen data={data} ghost={ghost} />
        </div>
      ) : (
        <div
          id="github-view-status"
          role="tabpanel"
          aria-labelledby="github-tab-status"
          className="github-view"
        >
          {!empty && <StatTiles data={data} />}

          {empty ? (
            card ? null : (
              <EmptyState
                testId="github-empty"
                title="No GitHub activity yet"
                why="The mirror has no PRs, issues, checks, artifacts, releases or branches for this project's repo yet."
              />
            )
          ) : (
            <div
              className={`github-status__panels${ghost ? " github-status__panels--ghost" : ""}`}
              data-testid="github-panels"
              data-ghost={ghost ? "true" : "false"}
              aria-hidden={ghost ? "true" : undefined}
            >
              <PRPanel prs={data.pullRequests} />
              <IssuePanel issues={data.issues} />
              <CheckPanel checks={data.checkRuns} />
              <ArtifactPanel artifacts={data.artifacts} />
              <ReleasePanel releases={data.releases} />
              <BranchPanel branches={data.branches} />
            </div>
          )}
        </div>
      )}
    </section>
  );
}

// ============================================================================
// Sync state cards (DESIGN-SPEC §3) — one per sync.reason, copy verbatim.
// ============================================================================

type StateCardDescriptor = {
  reason: string;
  tone: ChipTone;
  headline: string;
  body: string;
  cta: string;
  ctaKind: "retry" | "settings";
  ghost: boolean; // keep last-good data ghosted behind this card
};

/** stateCardFor returns the banner card for a non-healthy sync.reason, or null
 * when the mirror is Synced and within the freshness SLO (the happy path shows
 * no card). Copy is verbatim from DESIGN-SPEC §3 so the error UX matches the
 * observability taxonomy exactly. */
function stateCardFor(sync: GithubSync): StateCardDescriptor | null {
  const chip = chipState(sync);
  switch (sync.reason) {
    case SyncReason.NotConfigured:
      return {
        reason: sync.reason,
        tone: "neutral",
        headline: "No repository linked",
        body: "Link a GitHub repository to mirror its branches, pull requests and checks here. Once linked, sync is automatic — webhook plus a 5-minute poll, no manual kick.",
        cta: "Link GitHub repository",
        ctaKind: "settings",
        ghost: false,
      };
    case SyncReason.CredentialMissing:
      return {
        reason: sync.reason,
        tone: "blocked",
        headline: "GitHub token can't be resolved",
        body: "Sync is paused because the repository credential could not be resolved. The data below is the last known snapshot and may be stale — reconnect GitHub to resume mirroring.",
        cta: "Reconnect GitHub",
        ctaKind: "settings",
        ghost: true,
      };
    case SyncReason.ProviderError:
    case SyncReason.MirrorWriteError:
    case SyncReason.IssueSyncError:
      return {
        reason: sync.reason,
        tone: "paused",
        headline: "GitHub unreachable — retrying",
        body: "The last sync failed and is retrying automatically with backoff. The data below is the last good snapshot — the tab is never blank on a transient error.",
        cta: "Retry now",
        ctaKind: "retry",
        ghost: true,
      };
    case SyncReason.Synced:
    default:
      // Synced but behind the freshness SLO ⇒ "Data is behind schedule".
      if (chip.tone === "paused") {
        return {
          reason: sync.reason,
          tone: "paused",
          headline: "Data is behind schedule",
          body: "The mirror is older than the freshness target (SLO 6m). It refreshes on webhook and a 5-minute poll; refresh now if you can't wait.",
          cta: "Refresh now",
          ctaKind: "retry",
          ghost: false,
        };
      }
      return null;
  }
}

function StateCard({
  card,
  syncing,
  onRetry,
  projectId,
}: {
  card: StateCardDescriptor;
  syncing: boolean;
  onRetry: () => void;
  projectId: string;
}) {
  const toneClass = card.tone === "neutral" ? "" : ` github-state-card--${card.tone}`;
  return (
    <div
      className={`card github-state-card${toneClass}`}
      data-testid="github-state-card"
      data-reason={card.reason}
      role="status"
    >
      <h2 className="github-state-card__headline">{card.headline}</h2>
      <p className="muted">{card.body}</p>
      {card.ctaKind === "retry" ? (
        <button
          type="button"
          className="github-state-card__cta"
          onClick={onRetry}
          disabled={syncing}
          data-testid="github-state-card-cta"
        >
          {syncing ? "Refreshing…" : card.cta}
        </button>
      ) : (
        <a
          className="github-state-card__cta"
          href={`/projects/${encodeURIComponent(projectId)}/settings`}
          data-testid="github-state-card-cta"
        >
          {card.cta}
        </a>
      )}
    </div>
  );
}

// ============================================================================
// Stat tiles (DESIGN-SPEC §2) — snapshot counts from the mirror projection.
// ============================================================================

function StatTiles({ data }: { data: GithubStatus }) {
  const tiles: Array<{ label: string; value: number }> = [
    { label: "Branches", value: data.branches.length },
    { label: "Pull requests", value: data.pullRequests.length },
    { label: "Issues", value: data.issues.length },
    { label: "Checks", value: data.checkRuns.length },
  ];
  return (
    <div className="github-stat-tiles" data-testid="github-stat-tiles">
      {tiles.map((t) => (
        <div className="github-stat-tile" key={t.label} data-testid={`stat-${t.label.toLowerCase().replace(/\s+/g, "-")}`}>
          <span className="github-stat-tile__value">{t.value}</span>
          <span className="github-stat-tile__label muted">{t.label}</span>
        </div>
      ))}
    </div>
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
