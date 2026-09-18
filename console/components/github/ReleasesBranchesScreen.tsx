// components/github/ReleasesBranchesScreen.tsx — ISI-4675: the "Releases &
// Branches" GitHub screen (child of ISI-4662, "Github screen").
//
// Renders the scm-mirror projection's releases + branches to match the
// board-approved "Releases & Branches Management" mockup: a release timeline
// (tags + notes), release statistics, a release-notes generator hand-off, and a
// branch network — all in the IsItObservable dark palette.
//
// HONESTY (ADR-0013 §D4, carried from GitHubStatusTab): every number and
// timestamp comes from the mirror. The mirror carries no release body,
// contributor counts, branch-protection flags or branch base refs, so this
// screen never invents them — it shows only what the mirror actually holds and
// labels recency as "synced X ago". Every actionable entity deep-links to
// GitHub (`releases/tag/{tag}`, `tree/{branch}`).

import {
  ageLabel,
  type GithubBranch,
  type GithubRelease,
  type GithubStatus,
} from "@/lib/github-status";
import {
  GITHUB_REPO_LABEL,
  GITHUB_REPO_URL,
  githubBranchHref,
  githubReleaseHref,
} from "@/lib/github-links";
import "./github-screens.css";

/** Release lifecycle → badge label + modifier class. The mirror's states are
 * exactly draft | prerelease | published (repo_sync / github.go releaseState). */
function releaseState(state: string): { label: string; mod: string } {
  switch (state) {
    case "published":
      return { label: "Published", mod: "published" };
    case "prerelease":
      return { label: "Pre-release", mod: "prerelease" };
    case "draft":
      return { label: "Draft", mod: "draft" };
    default:
      return { label: state || "Unknown", mod: "draft" };
  }
}

/** Newest release first; releases without a mirror timestamp sort last. */
function byPublishedDesc(a: GithubRelease, b: GithubRelease): number {
  const ta = a.publishedAt ? Date.parse(a.publishedAt) : 0;
  const tb = b.publishedAt ? Date.parse(b.publishedAt) : 0;
  return tb - ta;
}

/** Default branch first, then alphabetical. */
function byBranch(a: GithubBranch, b: GithubBranch): number {
  if (a.default !== b.default) return a.default ? -1 : 1;
  return a.name.localeCompare(b.name);
}

/** "2 days ago" from a mirror ISO timestamp; null when absent/unparseable. */
function timeAgo(iso: string | undefined): string | null {
  if (!iso) return null;
  const then = Date.parse(iso);
  if (Number.isNaN(then)) return null;
  return ageLabel(Math.max(0, (Date.now() - then) / 1000));
}

/** Teal external-link glyph on every actionable title (mockup v2). */
function ExtGlyph() {
  return (
    <span className="gh-ext" aria-hidden="true">
      {" "}
      ↗
    </span>
  );
}

export function ReleasesBranchesScreen({
  data,
  ghost = false,
}: {
  data: GithubStatus;
  /** Stale/errored mirror: keep last-good data visible but ghosted (degrade,
   * don't blank — DESIGN-SPEC §3). */
  ghost?: boolean;
}) {
  const releases = [...data.releases].sort(byPublishedDesc);
  const branches = [...data.branches].sort(byBranch);
  const age = data.sync?.ageSeconds;
  const freshness = age === undefined ? "not synced yet" : `synced ${ageLabel(age)}`;

  const published = releases.filter((r) => r.state === "published").length;
  const prereleases = releases.filter((r) => r.state === "prerelease").length;
  const drafts = releases.filter((r) => r.state === "draft").length;

  return (
    <div
      className={`gh-screen${ghost ? " gh-screen--ghost" : ""}`}
      data-testid="gh-releases-branches"
      data-ghost={ghost ? "true" : "false"}
      aria-hidden={ghost ? "true" : undefined}
    >
      <header className="gh-screen__head">
        <div className="gh-screen__heading">
          <h2 className="gh-screen__title">Releases &amp; Branches Management</h2>
          <p className="gh-screen__sub">
            <span className="gh-screen__repo">{GITHUB_REPO_LABEL}</span>
            {` · ${releases.length} release${releases.length === 1 ? "" : "s"} · `}
            {`${branches.length} branch${branches.length === 1 ? "" : "es"} · `}
            <span data-testid="gh-freshness">{freshness}</span>
          </p>
        </div>
        <a
          className="gh-btn"
          href={GITHUB_REPO_URL}
          target="_blank"
          rel="noreferrer noopener"
          data-testid="gh-open-github"
        >
          Open on GitHub <ExtGlyph />
        </a>
      </header>

      <div className="gh-actions" data-testid="gh-actions">
        <a
          className="gh-btn gh-btn--green"
          href={`${GITHUB_REPO_URL}/releases/new`}
          target="_blank"
          rel="noreferrer noopener"
        >
          New Release
        </a>
        <a
          className="gh-btn gh-btn--blue"
          href={`${GITHUB_REPO_URL}/branches`}
          target="_blank"
          rel="noreferrer noopener"
        >
          New Branch
        </a>
        <a
          className="gh-btn"
          href={`${GITHUB_REPO_URL}/settings/branches`}
          target="_blank"
          rel="noreferrer noopener"
        >
          Protection Rules
        </a>
        <a
          className="gh-btn"
          href={`${GITHUB_REPO_URL}/settings`}
          target="_blank"
          rel="noreferrer noopener"
        >
          Merge Strategy
        </a>
      </div>

      <div className="gh-grid">
        <section className="gh-panel" data-testid="gh-release-timeline">
          <header className="gh-panel__head">
            <h3 className="gh-panel__title">Release Timeline</h3>
            <a
              className="gh-panel__action"
              href={`${GITHUB_REPO_URL}/releases`}
              target="_blank"
              rel="noreferrer noopener"
            >
              View all →
            </a>
          </header>
          <div className="gh-panel__body">
            {releases.length === 0 ? (
              <p className="gh-panel__empty" role="status" data-testid="gh-releases-empty">
                No releases mirrored yet.
              </p>
            ) : (
              <ul className="gh-releases" data-testid="gh-release-list">
                {releases.map((r, i) => (
                  <ReleaseRow key={`release-${r.tag || r.name}-${i}`} release={r} />
                ))}
              </ul>
            )}
          </div>
        </section>

        <section className="gh-panel" data-testid="gh-release-stats">
          <header className="gh-panel__head">
            <h3 className="gh-panel__title">Release Statistics</h3>
          </header>
          <div className="gh-panel__body">
            <div className="gh-stats">
              <Stat value={releases.length} label="Total Releases" sub="mirrored" tone="blue" />
              <Stat value={published} label="Published" sub="released" tone="green" />
              <Stat value={prereleases} label="Pre-releases" sub="flagged" tone="amber" />
              <Stat value={drafts} label="Drafts" sub="unpublished" tone="muted" />
              <Stat value={branches.length} label="Branches" sub="mirrored" tone="blue" />
            </div>
            <div className="gh-notes" data-testid="gh-notes-generator" style={{ marginTop: 14 }}>
              <div className="gh-notes__text">
                <h4 className="gh-notes__title">Release Notes Generator</h4>
                <p className="gh-notes__desc">
                  GitHub drafts notes automatically from commits and merged pull requests.
                </p>
              </div>
              <a
                className="gh-btn gh-btn--blue"
                href={`${GITHUB_REPO_URL}/releases/new`}
                target="_blank"
                rel="noreferrer noopener"
              >
                Generate on GitHub <ExtGlyph />
              </a>
            </div>
          </div>
        </section>
      </div>

      <section className="gh-panel" data-testid="gh-branch-network">
        <header className="gh-panel__head">
          <h3 className="gh-panel__title">Branch Network</h3>
          <a
            className="gh-panel__action"
            href={`${GITHUB_REPO_URL}/branches`}
            target="_blank"
            rel="noreferrer noopener"
          >
            Open branches on GitHub ↗
          </a>
        </header>
        <div className="gh-panel__body">
          {branches.length === 0 ? (
            <p className="gh-panel__empty" role="status" data-testid="gh-branches-empty">
              No branches mirrored yet.
            </p>
          ) : (
            <ul className="gh-branches" data-testid="gh-branch-list">
              {branches.map((b) => (
                <BranchNode key={`branch-${b.name}`} branch={b} />
              ))}
            </ul>
          )}
          <p className="gh-caption">
            Default branch and head commits are projected from GitHub. The mirror does not carry
            branch protection or base refs, so they are not shown.
          </p>
        </div>
      </section>
    </div>
  );
}

function ReleaseRow({ release }: { release: GithubRelease }) {
  const tag = release.tag || release.name;
  const state = releaseState(release.state);
  const href = githubReleaseHref(release.tag || release.name, release.url);
  const notes = release.tag && release.name && release.name !== release.tag ? release.name : "";
  const when = timeAgo(release.publishedAt);
  const meta =
    release.state === "draft"
      ? "Not published yet"
      : when
        ? `Released ${when}${release.actor ? ` by @${release.actor}` : ""}`
        : release.actor
          ? `Released by @${release.actor}`
          : "Release time not mirrored";

  return (
    <li className={`gh-release gh-release--${state.mod}`} data-testid="gh-release-row">
      <div className="gh-release__row">
        <a
          className="gh-release__tag"
          href={href}
          target="_blank"
          rel="noreferrer noopener"
          data-testid="gh-release-link"
        >
          {tag}
          <ExtGlyph />
        </a>
        <span className="gh-release__side">
          <span className={`gh-badge gh-badge--${state.mod}`} data-testid="gh-release-state">
            {state.label}
          </span>
          <a
            className="gh-release__download"
            href={href}
            target="_blank"
            rel="noreferrer noopener"
          >
            Download
          </a>
        </span>
      </div>
      {notes && <p className="gh-release__notes">{notes}</p>}
      <p className="gh-release__meta">{meta}</p>
    </li>
  );
}

function BranchNode({ branch }: { branch: GithubBranch }) {
  const href = githubBranchHref(branch.name);
  return (
    <li
      className={`gh-branch${branch.default ? " gh-branch--default" : ""}`}
      data-testid="gh-branch-row"
    >
      <a
        className="gh-branch__name"
        href={href}
        target="_blank"
        rel="noreferrer noopener"
        data-testid="gh-branch-link"
      >
        {branch.name}
        <ExtGlyph />
      </a>
      <span className="gh-branch__meta">
        {branch.default && <span className="gh-branch__badge">default</span>}
        {branch.headSha && <span className="gh-branch__sha">{branch.headSha.slice(0, 7)}</span>}
      </span>
    </li>
  );
}

function Stat({
  value,
  label,
  sub,
  tone,
}: {
  value: number;
  label: string;
  sub: string;
  tone: "green" | "amber" | "blue" | "muted";
}) {
  return (
    <div className={`gh-stat gh-stat--${tone}`} data-testid={`gh-stat-${label.toLowerCase().replace(/\s+/g, "-")}`}>
      <span className="gh-stat__value">{value}</span>
      <span className="gh-stat__label">{label}</span>
      <span className="gh-stat__label">{sub}</span>
    </div>
  );
}
