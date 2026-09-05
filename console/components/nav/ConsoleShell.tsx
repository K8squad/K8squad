"use client";

// components/nav/ConsoleShell.tsx — the ONE adaptive nav shell (stories 8.13 + 8.20 / ADR-038).
//
// CSS-first single tree: the server renders ONE shell; the browser re-expresses it at each
// canonical breakpoint — desktop >1024 full-width top bar + labeled left rail · tablet 768–1024
// the same top bar + collapsible icon rail (tap-to-expand OVERLAY — no layout shift, no bottom
// nav) · mobile <768 top bar + 5-item bottom nav + slide-in drawer for overflow. No UA sniffing;
// the variants below are all in the DOM and globals.css media queries decide — the same markup,
// same BFF/RBAC/SSE path.
//
// ISI-3871 (ISI-3867 #2/#3, mocks ISI-3641 frames 01/06): desktop/tablet carry a FULL-WIDTH top
// bar (`.appbar`, h=56) spanning rail+content — 8-Crest logo + K8squad wordmark + muted
// `console · <host>` env chip on the left, GlobalSearch + username + avatar on the right. The
// left rail is NAV-ONLY below it: brand block removed (it lives in the appbar now), active item
// = filled accent-tinted pill with a 3px accent bar per the mock.
//
// RBAC composition (8.16 seam): nodes a role cannot see are removed by visibleNav BEFORE any
// breakpoint expression, so a non-admin on mobile sees the role-filtered surface fit the
// 5-item budget with overflow spilling to the drawer (story 8.20 composition AC). The role
// itself arrives with Epic 15.4; until then `access` defaults to "user". Hidden = absent from
// the DOM (filtering happens here), never display:none-as-authz.

import { useEffect, useState, type ReactNode } from "react";
import Link from "next/link";
import { usePathname } from "next/navigation";
import { Logo } from "@/components/Logo";
import { ThemeToggle } from "@/components/ThemeToggle";
import { GlobalSearch } from "@/components/search/GlobalSearch";
import { NavIcon } from "@/components/nav/NavIcon";
import { NavigatingProjectSelector } from "@/components/nav/ProjectSelector";
import { Breadcrumb } from "@/components/nav/Breadcrumb";
import { UserMenu } from "@/components/nav/UserMenu";
import { SetupChip } from "@/components/nav/SetupChip";
import {
  mobileNav,
  navTree,
  visibleNav,
  withOnboardingLock,
  withProject,
  type AccessLevel,
  type NavNode,
  type OnboardingProgress,
} from "@/lib/nav";

function activeIds(pathname: string): Set<string> {
  const ids = new Set<string>();
  if (pathname.startsWith("/overview")) ids.add("overview");
  if (pathname.startsWith("/compose")) ids.add("compose");
  if (pathname.startsWith("/teams")) ids.add("teams");
  if (pathname.startsWith("/projects")) ids.add("projects");
  if (pathname.startsWith("/agents")) ids.add("agents");
  if (pathname.startsWith("/runs")) ids.add("runs");
  if (pathname.startsWith("/credentials")) ids.add("credentials");
  if (pathname.startsWith("/plugins")) ids.add("plugins");
  if (pathname.startsWith("/users")) ids.add("users");
  // "OTel" is the rail label for the OTLP config surface at /settings/configuration (ISI-3725):
  // the whole /settings subtree lights the OTel item, keeping active-nav honest until Settings grows
  // more than one surface.
  if (pathname.startsWith("/settings")) ids.add("otel");
  // Project sub-nav tab active-state (ISI-3651 E5) still derives from the deep project route.
  const m = pathname.match(/^\/projects\/[^/]+\/(\w+)/);
  if (m) ids.add(m[1]);
  return ids;
}

function NodeLink({
  node,
  active,
  projectId,
  onNavigate,
  expanded,
}: {
  node: NavNode;
  active: boolean;
  projectId: string | null;
  onNavigate?: () => void;
  expanded?: boolean;
}) {
  // E1-S3 soft lock (AD-10, FR-1.4): a locked node stays VISIBLE but renders as inert
  // text with a padlock glyph — never a link, never pruned from the DOM. The text
  // equivalent (NFR-4) is the aria-label + sr-only hint; the title gives sighted users
  // the same reason on hover.
  if (node.locked) {
    return (
      <span
        className="rail__link rail__link--locked"
        data-expanded={expanded}
        aria-disabled="true"
        aria-label={`${node.label} (locked — finish setup to unlock)`}
        title="Locked until your first Team exists — finish setup to unlock"
      >
        <span className="rail__icon">
          <NavIcon id={node.id} />
        </span>
        <span className="rail__label">{node.label}</span>
        <span className="rail__lock" aria-hidden="true">
          <NavIcon id="lock" size={13} />
        </span>
      </span>
    );
  }
  const href = node.scope === "project" && projectId
    ? withProject(node.href, projectId)
    : node.scope === "project"
      ? "/overview" // no project selected yet — Project root lands on a global screen
      : node.href;
  return (
    <Link
      href={href}
      className="rail__link"
      data-active={active || undefined}
      data-expanded={expanded}
      onClick={onNavigate}
      aria-current={active ? "page" : undefined}
      // The label span is display:none on the collapsed tablet rail and NavIcon's svg is
      // aria-hidden, so without this the link would have no accessible name. Matches the
      // visible label exactly, so no name mismatch when the text is shown.
      aria-label={node.label}
    >
      <span className="rail__icon">
        <NavIcon id={node.id} />
      </span>
      <span className="rail__label">{node.label}</span>
      {node.badge && <span className="rail__badge">{node.badge}</span>}
    </Link>
  );
}

export function ConsoleShell({
  children,
  access = "user",
  username = null,
  tree = navTree(),
}: {
  children: ReactNode;
  access?: AccessLevel;
  username?: string | null;
  tree?: NavNode[];
}) {
  const pathname = usePathname() ?? "/";
  const [railExpanded, setRailExpanded] = useState(false);
  const [drawerOpen, setDrawerOpen] = useState(false);
  // E1-S3: the AD-2 onboarding projection drives the nav lock + "Finish setup" chip.
  const [progress, setProgress] = useState<OnboardingProgress | null>(null);

  // ISI-3871: the appbar's env chip reads `console · <host>` from the CURRENT origin at render
  // time (mock frame 01/06 literal is `console · 10.0.0.219`). SSR-safe: the first paint uses
  // the NEXT_PUBLIC_CONSOLE_HOST build-time fallback (empty string when unset — the chip then
  // reads just `console`), and the effect swaps in window.location.hostname AFTER hydration so
  // server and client's first render agree (no hydration mismatch), then it becomes exact.
  const [consoleHost, setConsoleHost] = useState(
    process.env.NEXT_PUBLIC_CONSOLE_HOST ?? "",
  );

  // Close transient surfaces on navigation.
  useEffect(() => {
    setRailExpanded(false);
    setDrawerOpen(false);
  }, [pathname]);

  useEffect(() => {
    setConsoleHost(window.location.hostname);
  }, []);

  // Read the onboarding projection once per mount (E1-S1 BFF proxy). Fail-open: if the
  // endpoint is unreachable, unauthenticated, or not yet wired (E1-S1 still landing), the
  // nav simply renders unlocked with no chip — the lock is a courtesy rail, never an
  // authz wall (the apiserver's gates stay the real wall).
  useEffect(() => {
    let cancelled = false;
    fetch("/api/onboarding/progress", { cache: "no-store" })
      .then((res) => (res.ok ? res.json() : null))
      .then((p) => {
        if (cancelled || !p || typeof p.done !== "number" || typeof p.total !== "number") {
          return;
        }
        setProgress(p as OnboardingProgress);
      })
      .catch(() => {});
    return () => {
      cancelled = true;
    };
  }, []);

  const ids = activeIds(pathname);
  // FR-1.4: until a Team exists (milestone ① is always the first incomplete one when it
  // doesn't, so nextMilestone === "team"), gate the non-setup surfaces with the soft lock.
  const teamExists = progress ? progress.nextMilestone !== "team" : true;
  const lockedTree = withOnboardingLock(tree, teamExists);
  const nodes = visibleNav(lockedTree, access);
  const projectMatch = pathname.match(/^\/projects\/([^/]+)/);
  const activeProject = projectMatch ? decodeURIComponent(projectMatch[1]) : null;
  const { bottom, drawer } = mobileNav(access, lockedTree);

  return (
    <div className="shell">
      {/* Desktop/tablet FULL-WIDTH top bar (ISI-3871 / ISI-3641 frames 01+06, h=56, spans rail
          and content): brand lockup + `console · <host>` env chip left · search + username +
          avatar right. Hidden on mobile (<768), where `.topbar` (below) keeps story 8.20 intact. */}
      <header className="appbar">
        <Link href="/overview" className="appbar__brand" aria-label="K8squad home">
          <Logo size={24} />
        </Link>
        {/* Muted env chip — the mock's `console · 10.0.0.219`. Host is the live origin
            (see consoleHost above); empty host degrades to just `console`. */}
        <span className="appbar__env" title="Console host">
          console{consoleHost ? ` · ${consoleHost}` : ""}
        </span>
        <span className="appbar__spacer" />
        <GlobalSearch />
        {username && <span className="appbar__user">{username}</span>}
        {/* Identity indicator (mock's top-right `admin` + avatar circle). Sign-out stays in the
            rail foot / drawer (ISI-3570) — avatar-only reuse of UserMenu, not a second control. */}
        <UserMenu username={username} variant="avatar" />
      </header>

      <div className="shell__row">
      {/* Desktop/tablet left rail (labels on desktop; icon-only on tablet, tap expands overlay).
          ISI-3871: NAV-ONLY — the brand block moved up into the appbar; only the tablet expand
          affordance remains at the rail's head (hidden on desktop where the rail is persistent). */}
      <aside
        className="rail"
        data-expanded={railExpanded || undefined}
        aria-label="Primary"
      >
        <div className="rail__head">
          <button
            type="button"
            className="rail__expand"
            aria-label={railExpanded ? "Collapse navigation" : "Expand navigation"}
            aria-expanded={railExpanded}
            onClick={() => setRailExpanded((v) => !v)}
          >
            <NavIcon id={railExpanded ? "close" : "menu"} />
          </button>
        </div>
        <nav className="rail__nav">
          {nodes.map((n) =>
            n.section ? (
              // SETTINGS group (ISI-3725): a small-caps heading + its children as full rail links,
              // NOT the project accordion. The heading is presentational (no link, no icon).
              <div key={n.id} className="rail__group rail__group--section">
                <div className="rail__section">{n.label}</div>
                {(n.children ?? []).map((c) => (
                  <NodeLink
                    key={c.id}
                    node={c}
                    active={ids.has(c.id)}
                    projectId={activeProject}
                  />
                ))}
              </div>
            ) : (
              <div key={n.id} className="rail__group">
                <NodeLink
                  node={n}
                  active={ids.has(n.id)}
                  projectId={activeProject}
                />
                {n.children && (n.id !== "project" || activeProject) && (
                  <div className="rail__sub">
                    {n.children.map((c) => (
                      <NodeLink
                        key={c.id}
                        node={c}
                        active={ids.has(c.id)}
                        projectId={activeProject}
                        expanded
                      />
                    ))}
                  </div>
                )}
              </div>
            ),
          )}
        </nav>
        {/* Functional deviations the mock predates (ISI-3871): ProjectSelector + SetupChip stay,
            but restyled to sit QUIETLY BELOW the mock's nav geometry — the rail top now reads
            exactly like frames 01/06 (nav first, then SETTINGS). */}
        <NavigatingProjectSelector activeId={activeProject} />
        {/* E1-S3: persistent way back to setup (renders only when incomplete + dismissed). */}
        <SetupChip progress={progress} />
        {/* Account footer + sign-out (ISI-3570) — always at the rail's foot. */}
        <div className="rail__foot">
          <UserMenu username={username} variant="rail" />
        </div>
      </aside>
      {railExpanded && (
        <div
          className="rail__scrim"
          role="presentation"
          onClick={() => setRailExpanded(false)}
        />
      )}

      <div className="shell__body">
        {/* Mobile top bar */}
        <header className="topbar">
          <button
            type="button"
            className="topbar__menu"
            aria-label="Open navigation"
            onClick={() => setDrawerOpen(true)}
          >
            <NavIcon id="menu" size={20} />
          </button>
          <Logo size={22} />
          <GlobalSearch />
          <ThemeToggle />
        </header>

        {/* Desktop/tablet content header — a slim breadcrumb strip (ISI-3871). The mock elements
            (search · username · avatar) moved UP into the full-width appbar; Breadcrumb +
            ThemeToggle stay as the recorded product decisions (ISI-3716 §2) and the namespace
            chip remains a quiet functional indicator — none of them displace mock elements. */}
        <header className="contentbar">
          <Breadcrumb pathname={pathname} />
          <span className="nschip" title="Namespace scope">
            <span className="nschip__dot" aria-hidden="true">
              ●
            </span>
            ns: all
          </span>
          <ThemeToggle />
        </header>

        <main className="shell__main">{children}</main>
      </div>
      </div>

      {/* Mobile 5-item bottom nav (story 8.20) — role-filtered, overflow → drawer */}
      <nav className="bottomnav" aria-label="Primary mobile">
        {bottom.map((n) => (
          <NodeLink key={n.id} node={n} active={ids.has(n.id)} projectId={activeProject} />
        ))}
      </nav>

      {/* Mobile slide-in drawer for overflow (project sub-nav, settings children, past-budget) */}
      {drawerOpen && (
        <div className="drawer" role="dialog" aria-modal="true" aria-label="Navigation">
          <div className="drawer__panel">
            <div className="drawer__head">
              <Logo size={22} />
              <button
                type="button"
                className="drawer__close"
                aria-label="Close navigation"
                onClick={() => setDrawerOpen(false)}
              >
                <NavIcon id="close" size={20} />
              </button>
            </div>
            <div className="drawer__body">
              {drawer.map((n) =>
                n.section ? (
                  <div key={`d-${n.id}`} className="rail__section">
                    {n.label}
                  </div>
                ) : (
                  <NodeLink
                    key={`d-${n.id}`}
                    node={n}
                    active={ids.has(n.id)}
                    projectId={activeProject}
                    onNavigate={() => setDrawerOpen(false)}
                    expanded
                  />
                ),
              )}
            </div>
            {/* E1-S3 chip, also reachable on mobile. */}
            <SetupChip progress={progress} onNavigate={() => setDrawerOpen(false)} />
            {/* Sign-out also reachable from the mobile drawer (ISI-3570). */}
            <div className="drawer__foot">
              <UserMenu username={username} variant="drawer" />
            </div>
          </div>
          <div
            className="drawer__scrim"
            role="presentation"
            onClick={() => setDrawerOpen(false)}
          />
        </div>
      )}
    </div>
  );
}
