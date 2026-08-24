"use client";

// components/nav/ConsoleShell.tsx — the ONE adaptive nav shell (stories 8.13 + 8.20 / ADR-038).
//
// CSS-first single tree: the server renders ONE shell; the browser re-expresses it at each
// canonical breakpoint — desktop >1024 full labeled left rail · tablet 768–1024 collapsible
// icon rail (tap-to-expand OVERLAY — no layout shift, no bottom nav) · mobile <768 top bar +
// 5-item bottom nav + slide-in drawer for overflow. No UA sniffing; the variants below are all
// in the DOM and globals.css media queries decide — the same markup, same BFF/RBAC/SSE path.
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
import { NavIcon } from "@/components/nav/NavIcon";
import { NavigatingProjectSelector } from "@/components/nav/ProjectSelector";
import { Breadcrumb } from "@/components/nav/Breadcrumb";
import {
  mobileNav,
  navTree,
  visibleNav,
  withProject,
  type AccessLevel,
  type NavNode,
} from "@/lib/nav";

function activeIds(pathname: string): Set<string> {
  const ids = new Set<string>();
  if (pathname === "/") ids.add("dashboard");
  if (pathname.startsWith("/overview")) ids.add("overview");
  if (pathname.startsWith("/agents")) ids.add("agents");
  const m = pathname.match(/^\/projects\/([^/]+)(?:\/(\w+))?/);
  if (m) {
    ids.add("project");
    if (m[2]) ids.add(m[2]);
  }
  const s = pathname.match(/^\/settings(?:\/(\w+))?/);
  if (s) {
    ids.add("settings");
    if (s[1]) ids.add(s[1]);
  }
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
    >
      <span className="rail__icon">
        <NavIcon id={node.id} />
      </span>
      <span className="rail__label">{node.label}</span>
    </Link>
  );
}

export function ConsoleShell({
  children,
  access = "user",
  tree = navTree(),
}: {
  children: ReactNode;
  access?: AccessLevel;
  tree?: NavNode[];
}) {
  const pathname = usePathname() ?? "/";
  const [railExpanded, setRailExpanded] = useState(false);
  const [drawerOpen, setDrawerOpen] = useState(false);

  // Close transient surfaces on navigation.
  useEffect(() => {
    setRailExpanded(false);
    setDrawerOpen(false);
  }, [pathname]);

  const ids = activeIds(pathname);
  const nodes = visibleNav(tree, access);
  const projectMatch = pathname.match(/^\/projects\/([^/]+)/);
  const activeProject = projectMatch ? decodeURIComponent(projectMatch[1]) : null;
  const { bottom, drawer } = mobileNav(access, tree);

  return (
    <div className="shell">
      {/* Desktop/tablet left rail (labels on desktop; icon-only on tablet, tap expands overlay) */}
      <aside
        className="rail"
        data-expanded={railExpanded || undefined}
        aria-label="Primary"
      >
        <div className="rail__brand">
          <Link href="/" className="rail__homelink">
            <Logo size={24} />
          </Link>
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
        <NavigatingProjectSelector activeId={activeProject} />
        <nav className="rail__nav">
          {nodes.map((n) => (
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
          ))}
        </nav>
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
          <ThemeToggle />
        </header>

        {/* Desktop/tablet content header */}
        <header className="contentbar">
          <Breadcrumb pathname={pathname} />
          <ThemeToggle />
        </header>

        <main className="shell__main">{children}</main>
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
              {drawer.map((n) => (
                <NodeLink
                  key={`d-${n.id}`}
                  node={n}
                  active={ids.has(n.id)}
                  projectId={activeProject}
                  onNavigate={() => setDrawerOpen(false)}
                  expanded
                />
              ))}
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
