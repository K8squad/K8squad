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
import { CiCdPipelineStatus } from "@/components/github/CiCdPipelineStatus";
import { PullRequestManagement } from "@/components/github/PullRequestManagement";
import {
  ageLabel,
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

// The repo the GitHub screen deep-links to (ISI-4671). The mirror read model does
// not expose the repo slug, so the screen uses the known k8squad repo target from
// the approved mockup; entity-level links use the normalized mirror URLs.
const GITHUB_REPO_URL = "https://github.com/K8squad/K8squad";
const REPO_SLUG = "K8squad/K8squad";
// Timeline is capped so the overview stays a dashboard, not an infinite list.
const TIMELINE_LIMIT = 6;

export function GitHubStatusTab({ projectId }: { projectId: string }) {
  const [state, setState] = useState<GithubStatusState>({ kind: "loading" });
  const [syncing, setSyncing] = useState(false);
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
      <section className="github-status" aria-busy="true" data-testid="github-loading">
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
    <section className="github-status" data-testid="github-status">
      <header className="github-status__head">
        <div className="github-status__title">
          <h1>{data.project.name}</h1>
          <a
            className="github-repo-slug"
            href={GITHUB_REPO_URL}
            target="_blank"
            rel="noreferrer noopener"
            data-testid="github-repo-link"
            title="Open the repository on GitHub"
          >
            {REPO_SLUG}
            <span className="github-ext" aria-hidden="true">
              ↗
            </span>
          </a>
        </div>
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
        <a
          className="github-open-github"
          href={GITHUB_REPO_URL}
          target="_blank"
          rel="noreferrer noopener"
          data-testid="github-open-github"
        >
          Open on GitHub
          <span className="github-ext" aria-hidden="true">
            ↗
          </span>
        </a>
      </header>

      {chip.tone === "running" && (
        <p className="muted github-status__autoline" data-testid="github-autoline">
          ⚡ Auto-refreshed — webhook + 5-min poll · no manual kick
        </p>
      )}

      {card && (
        <StateCard card={card} syncing={syncing} onRetry={() => void handleSync()} projectId={projectId} />
      )}

      {!empty && <OverviewDashboard data={data} />}

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
          <PullRequestManagement data={data} ghost={ghost} />
          <IssuePanel issues={data.issues} />
          <CiCdPipelineStatus data={data} />
          <ReleasePanel releases={data.releases} />
          <BranchPanel branches={data.branches} />
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
// Overview Dashboard (ISI-4671) — activity metrics, recent-activity timeline and
// repo-health indicators, derived from the mirror projection. The layout matches
// the approved mockup: a 5-up metric band, a timeline, and a health sidebar.
//
// Fabrication discipline (FR-I3): every number comes from a real mirror row. A
// rate with no denominator renders "—", never a fake 0, and the composite health
// score is the mean of only the signals that actually had data.
// ============================================================================

type MetricTone = "teal" | "green" | "orange" | "purple";
type EventTone = "green" | "red" | "blue";

type PrCounts = { open: number; merged: number; closed: number };

function prCounts(prs: GithubStatus["pullRequests"]): PrCounts {
  const counts: PrCounts = { open: 0, merged: 0, closed: 0 };
  for (const pr of prs) {
    const state = (pr.state || "").toLowerCase();
    if (pr.merged || state === "merged") counts.merged += 1;
    else if (state === "closed") counts.closed += 1;
    else if (state === "open") counts.open += 1;
  }
  return counts;
}

function checkCounts(checks: GithubStatus["checkRuns"]) {
  const completed = checks.filter(
    (c) => (c.conclusion && c.conclusion.length > 0) || (c.state || "").toLowerCase() === "completed",
  );
  const passed = completed.filter((c) => (c.conclusion || "").toLowerCase() === "success");
  return { total: checks.length, completed: completed.length, passed: passed.length };
}

/** A rate in [0,1], or null when there is no denominator — the honest absence. */
function rate(numerator: number, denominator: number): number | null {
  if (denominator <= 0) return null;
  return numerator / denominator;
}

function pct(r: number | null): string {
  return r === null ? "—" : `${Math.round(r * 100)}%`;
}

type HealthSignal = { label: string; value: string; tone: MetricTone };

function healthTier(score: number | null): string {
  if (score === null) return "Not enough data";
  if (score >= 0.9) return "Excellent";
  if (score >= 0.75) return "Good";
  if (score >= 0.5) return "Fair";
  return "Needs attention";
}

/** Repo health from the signals the mirror can actually prove: PR merge rate,
 * CI success rate and issue close rate. The composite score is their mean. */
function repoHealth(data: GithubStatus): {
  score: number | null;
  tier: string;
  signals: HealthSignal[];
} {
  const prs = prCounts(data.pullRequests);
  const checks = checkCounts(data.checkRuns);
  const openIssues = data.issues.filter((i) => (i.state || "").toLowerCase() === "open").length;

  const mergeRate = rate(prs.merged, prs.merged + prs.closed);
  const ciRate = rate(checks.passed, checks.completed);
  const issueRate = rate(data.issues.length - openIssues, data.issues.length);

  const signals: HealthSignal[] = [
    { label: "PR merge rate", value: pct(mergeRate), tone: "green" },
    { label: "CI success rate", value: pct(ciRate), tone: "green" },
    { label: "Issue close rate", value: pct(issueRate), tone: "green" },
  ];
  const available = [mergeRate, ciRate, issueRate].filter((r): r is number => r !== null);
  const score =
    available.length === 0 ? null : available.reduce((a, b) => a + b, 0) / available.length;
  return { score, tier: healthTier(score), signals };
}

type ActivityEvent = {
  key: string;
  kind: "pr" | "issue" | "release";
  action: "opened" | "merged" | "closed" | "published";
  title: string;
  url?: string;
  actor?: string;
  at?: string;
  tone: EventTone;
};

/** Flatten the mirror rows into a single newest-first activity feed. Rows with a
 * mirror timestamp sort by it; rows without one keep projection order at the
 * tail and render "time unknown" rather than an invented time. */
function activityEvents(data: GithubStatus): ActivityEvent[] {
  const events: ActivityEvent[] = [];

  for (const pr of data.pullRequests) {
    const state = (pr.state || "").toLowerCase();
    const merged = pr.merged || state === "merged";
    events.push({
      key: `pr-${pr.number}`,
      kind: "pr",
      action: merged ? "merged" : state === "closed" ? "closed" : "opened",
      title: `PR #${pr.number}: ${pr.title}`,
      url: pr.url,
      actor: pr.actor,
      at: pr.updatedAt,
      tone: merged ? "green" : state === "closed" ? "red" : "blue",
    });
  }

  for (const it of data.issues) {
    const closed = (it.state || "").toLowerCase() === "closed";
    events.push({
      key: `issue-${it.number}`,
      kind: "issue",
      action: closed ? "closed" : "opened",
      title: `Issue #${it.number}: ${it.title}`,
      url: it.url,
      actor: it.actor,
      at: it.updatedAt,
      tone: closed ? "red" : "blue",
    });
  }

  for (const r of data.releases) {
    events.push({
      key: `release-${r.tag || r.name}`,
      kind: "release",
      action: "published",
      title: r.tag && r.name && r.tag !== r.name ? `${r.name} · ${r.tag}` : r.tag || r.name,
      url: r.url,
      actor: r.actor,
      at: r.publishedAt,
      tone: "green",
    });
  }

  return events
    .map((e, i) => ({ e, i }))
    .sort((a, b) => {
      const ta = a.e.at ? Date.parse(a.e.at) : Number.NaN;
      const tb = b.e.at ? Date.parse(b.e.at) : Number.NaN;
      const va = Number.isNaN(ta) ? Number.NEGATIVE_INFINITY : ta;
      const vb = Number.isNaN(tb) ? Number.NEGATIVE_INFINITY : tb;
      return vb - va || a.i - b.i;
    })
    .map(({ e }) => e);
}

function agoFrom(iso: string | undefined): string {
  if (!iso) return "time unknown";
  const then = Date.parse(iso);
  if (Number.isNaN(then)) return "time unknown";
  return ageLabel((Date.now() - then) / 1000);
}

function OverviewDashboard({ data }: { data: GithubStatus }) {
  const prs = prCounts(data.pullRequests);
  const checks = checkCounts(data.checkRuns);
  const openIssues = data.issues.filter((i) => (i.state || "").toLowerCase() === "open").length;
  const closedIssues = data.issues.length - openIssues;
  const defaultBranch = data.branches.find((b) => b.default);
  const latestRelease = data.releases[0];
  const health = repoHealth(data);
  const events = activityEvents(data).slice(0, TIMELINE_LIMIT);

  const metrics: Array<{
    label: string;
    value: number;
    tone: MetricTone;
    sub: string;
    testId: string;
  }> = [
    {
      label: "Branches",
      value: data.branches.length,
      tone: "teal",
      sub: defaultBranch ? `${defaultBranch.name} default` : "no default branch",
      testId: "stat-branches",
    },
    {
      label: "Open PRs",
      value: prs.open,
      tone: "green",
      sub: `${prs.merged} merged · ${prs.closed} closed`,
      testId: "stat-open-prs",
    },
    {
      label: "Open Issues",
      value: openIssues,
      tone: "orange",
      sub: `${closedIssues} closed`,
      testId: "stat-open-issues",
    },
    {
      label: "Check Runs",
      value: checks.total,
      tone: "purple",
      sub: checks.completed > 0 ? `${checks.passed} passing` : "no completed runs",
      testId: "stat-check-runs",
    },
    {
      label: "Releases",
      value: data.releases.length,
      tone: "orange",
      sub: latestRelease ? latestRelease.tag || latestRelease.name : "none published",
      testId: "stat-releases",
    },
  ];

  return (
    <div className="github-overview" data-testid="github-overview">
      <div className="github-stat-tiles" data-testid="github-stat-tiles">
        {metrics.map((m) => (
          <div className="github-stat-tile" key={m.label} data-testid={m.testId}>
            <span className={`github-stat-tile__value github-tone--${m.tone}`}>{m.value}</span>
            <span className="github-stat-tile__label muted">{m.label}</span>
            <span className="github-stat-tile__sub muted">{m.sub}</span>
          </div>
        ))}
      </div>

      <div className="github-overview__grid">
        <section className="card github-timeline" data-testid="github-timeline">
          <header className="github-timeline__head">
            <h2>Recent Activity</h2>
            <span className="muted">Newest first</span>
          </header>
          {events.length === 0 ? (
            <p className="muted" data-testid="github-timeline-empty">
              No activity recorded in the mirror yet.
            </p>
          ) : (
            <ul className="github-timeline__list">
              {events.map((e) => (
                <li
                  className="github-timeline__row"
                  key={e.key}
                  data-testid="github-timeline-row"
                  data-kind={e.kind}
                >
                  <span
                    className={`github-timeline__dot github-tone--bg-${e.tone}`}
                    aria-hidden="true"
                  />
                  <div className="github-timeline__body">
                    <EntityLink url={e.url}>{e.title}</EntityLink>
                    <span className="github-timeline__meta muted">
                      {e.action}
                      {e.actor ? ` by @${e.actor}` : ""} · {agoFrom(e.at)}
                    </span>
                  </div>
                  <span className={`github-timeline__badge github-tone--bg-${e.tone}`}>
                    {e.action}
                  </span>
                </li>
              ))}
            </ul>
          )}
        </section>

        <aside className="card github-health" data-testid="github-health">
          <h2 className="github-health__title">Repository Health</h2>
          <div className="github-health__score">
            <span className="github-health__value" data-testid="health-score">
              {health.score === null ? "—" : `${Math.round(health.score * 100)}%`}
            </span>
            <span className="github-health__tier muted" data-testid="health-tier">
              {health.tier}
            </span>
          </div>
          <dl className="github-health__signals">
            {health.signals.map((s) => (
              <div className="github-health__signal" key={s.label}>
                <dt className="muted">{s.label}</dt>
                <dd className={`github-health__signal-value github-tone--${s.tone}`}>{s.value}</dd>
              </div>
            ))}
          </dl>
          <p className="github-health__note muted">
            Derived from mirrored PRs, checks and issues — no external calls.
          </p>
        </aside>
      </div>
    </div>
  );
}

/** A GitHub deep-link with the teal external-link glyph, or plain text when the
 * normalized mirror url is absent (never a link to nowhere). */
function EntityLink({ url, children }: { url?: string; children: ReactNode }) {
  return (
    <Link url={url}>
      {children}
      {url && (
        <span className="github-ext" aria-hidden="true">
          {" "}
          ↗
        </span>
      )}
    </Link>
  );
}

function honest(title: string, why: string) {
  return (
    <section className="github-status" data-testid="github-status">
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
