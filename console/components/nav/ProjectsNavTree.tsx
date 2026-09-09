"use client";

// components/nav/ProjectsNavTree.tsx — the Projects rail sub-tree (ISI-4090).
//
// The rail's SECOND dynamic, lazy-loaded sub-tree (after Teams / ISI-4001). `navTree()` stays pure
// and static (lib/nav.ts); this scoped client island is mounted by ConsoleShell where the Projects
// node's accordion would render (marker: NavNode.dynamicChildren === "projects"). It expands in two
// levels:
//   Projects (link → /projects)  →  project(s)  →  that project's Build · Tickets · Runs ·
//   Discussion · GitHub sections (each an icon'd link → /projects/{id}/{section}).
//
// This replaces the old unstyled top-of-page sub-nav strip (projects/[projectId]/layout.tsx) — the
// left rail now owns project navigation, so the tab strip is gone.
//
// Data REUSE-ONLY (no new backend): projects = GET /api/projects (story 8.13 BFF, the same list the
// ProjectSelector reads — {id,name}; upstream scoping is enforced server-side, the island renders
// whatever it returns and never synthesizes a project). The 5 sections are STATIC — projectSubnav()
// from lib/nav.ts — so, unlike TeamsNavTree, expanding a project needs NO second fetch.
//
// URL-is-state: only expand/collapse lives in client state (top-level persisted per user via
// localStorage, mirroring TeamsNavTree). Which project + section is ACTIVE is a pure derivation of
// the pathname, and the active project is auto-expanded so a deep link always shows its section in
// context (SSR-safe: pathname is deterministic on server and client alike).
//
// Honesty: the sub-tree owns its outcome machine — loading spinner, neutral empty leaf, and an
// error with a retry affordance. A failure degrades inline; it never blanks or crashes the rest of
// the rail (ConsoleShell wraps this in an error boundary, falling back to the plain Projects link).

import { useCallback, useEffect, useState } from "react";
import Link from "next/link";
import { usePathname } from "next/navigation";
import { NavIcon } from "@/components/nav/NavIcon";
import { projectSubnav } from "@/lib/nav";
import type { ProjectOption } from "@/components/nav/ProjectSelector";

type ListState = "unopened" | "loading" | "ok" | "empty" | "error";

const PROJECTS_EXPAND_KEY = "ksquad.nav.projects.expanded";

export interface ProjectsNavTreeProps {
  /** True when the Projects node is the active nav (pathname → /projects); the URL still owns active. */
  active?: boolean;
  /** Pathname override for tests; defaults to usePathname(). */
  pathname?: string;
  /** Loader for the project list (BFF GET /api/projects). Injectable for tests. */
  loadProjects?: () => Promise<Response>;
  /** Start with Projects expanded (tests / deterministic SSR); overrides the persisted value. */
  defaultExpanded?: boolean;
}

const defaultLoadProjects = () => fetch("/api/projects", { cache: "no-store" });

/** The active `{ projectId, section }` embedded in a /projects/{id}/{section} route. Pure, URL-only. */
function activeProjectRoute(pathname: string): { projectId: string | null; section: string | null } {
  const m = pathname.match(/^\/projects\/([^/?#]+)(?:\/([^/?#]+))?/);
  return {
    projectId: m ? decodeURIComponent(m[1]) : null,
    section: m && m[2] ? m[2] : null,
  };
}

export function ProjectsNavTree({
  active = false,
  pathname: pathnameProp,
  loadProjects = defaultLoadProjects,
  defaultExpanded,
}: ProjectsNavTreeProps) {
  const routerPath = usePathname();
  const pathname = pathnameProp ?? routerPath ?? "/";
  const { projectId: activeProjectId, section: activeSection } = activeProjectRoute(pathname);

  const [expanded, setExpanded] = useState<boolean>(defaultExpanded ?? activeProjectId !== null);
  const [listState, setListState] = useState<ListState>("unopened");
  const [projects, setProjects] = useState<ProjectOption[]>([]);
  // The active project is auto-expanded so a deep link shows its section in context.
  const [expandedProjects, setExpandedProjects] = useState<Set<string>>(
    () => new Set(activeProjectId ? [activeProjectId] : []),
  );
  // Bumped to force a refetch on the retry affordance.
  const [nonce, setNonce] = useState(0);

  // Restore the persisted top-level expansion once, after mount (SSR-safe: the server renders from
  // the pathname alone). An explicit defaultExpanded or an active project both win over the store.
  useEffect(() => {
    if (defaultExpanded !== undefined || activeProjectId) return;
    try {
      if (localStorage.getItem(PROJECTS_EXPAND_KEY) === "1") setExpanded(true);
    } catch {
      /* storage blocked — stay collapsed, cosmetic only */
    }
  }, [defaultExpanded, activeProjectId]);

  // Keep the active project expanded as the URL changes (deep-link / in-app navigation).
  useEffect(() => {
    if (activeProjectId) setExpandedProjects((prev) => new Set(prev).add(activeProjectId));
  }, [activeProjectId]);

  const persistExpanded = useCallback((next: boolean) => {
    try {
      localStorage.setItem(PROJECTS_EXPAND_KEY, next ? "1" : "0");
    } catch {
      /* ignore */
    }
  }, []);

  // Fetch the project list on first expand (and on retry). Lazy: nothing is fetched while collapsed.
  useEffect(() => {
    if (!expanded) return;
    if (listState !== "unopened" && nonce === 0) return;
    let alive = true;
    setListState("loading");
    loadProjects()
      .then((res) => {
        if (!alive) return;
        if (res.status >= 200 && res.status < 300) {
          return res.json().then((body: unknown) => {
            if (!alive) return;
            const rows: ProjectOption[] = Array.isArray((body as { projects?: unknown })?.projects)
              ? ((body as { projects: ProjectOption[] }).projects)
              : [];
            const sorted = [...rows].sort((a, b) => a.name.localeCompare(b.name));
            setProjects(sorted);
            setListState(sorted.length === 0 ? "empty" : "ok");
          });
        }
        setListState("error");
      })
      .catch(() => alive && setListState("error"));
    return () => {
      alive = false;
    };
    // nonce drives retry; listState intentionally excluded to avoid a refetch loop.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [expanded, nonce, loadProjects]);

  const toggleProjects = useCallback(() => {
    setExpanded((v) => {
      const next = !v;
      persistExpanded(next);
      return next;
    });
  }, [persistExpanded]);

  const toggleProject = useCallback((id: string) => {
    setExpandedProjects((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  }, []);

  const retry = useCallback(() => setNonce((n) => n + 1), []);

  const projectsActive = active || activeProjectId !== null;

  return (
    <div className="rail__tree" data-testid="projects-nav-tree">
      {/* Projects row: the label links to /projects; a SEPARATE chevron toggles the sub-tree —
          two distinct, keyboard-operable affordances (mirrors TeamsNavTree). */}
      <div className="rail__treerow">
        <Link
          href="/projects"
          className="rail__link rail__link--haschild"
          data-active={projectsActive || undefined}
          aria-current={projectsActive ? "page" : undefined}
          aria-label="Projects"
        >
          <span className="rail__icon">
            <NavIcon id="projects" />
          </span>
          <span className="rail__label">Projects</span>
        </Link>
        <button
          type="button"
          className="rail__disclosure"
          aria-expanded={expanded}
          aria-controls="rail-projects-subtree"
          aria-label={expanded ? "Collapse Projects" : "Expand Projects"}
          data-testid="projects-toggle"
          onClick={toggleProjects}
        >
          <Caret open={expanded} />
        </button>
      </div>

      {expanded && (
        <div className="rail__sub" id="rail-projects-subtree" role="group" aria-label="Projects">
          {listState === "loading" && (
            <p className="rail__navstate" data-testid="projects-tree-loading" role="status">
              <span className="rail__spinner" aria-hidden="true" />
              Loading projects…
            </p>
          )}

          {listState === "error" && (
            <p className="rail__navstate" data-testid="projects-tree-error">
              <span>Couldn’t load projects.</span>
              <button type="button" className="rail__retry" onClick={retry}>
                Retry
              </button>
            </p>
          )}

          {listState === "empty" && (
            <p className="rail__navstate rail__navstate--muted" data-testid="projects-tree-empty">
              No projects
            </p>
          )}

          {listState === "ok" &&
            projects.map((project) => {
              const isOpen = expandedProjects.has(project.id);
              const projectOnPath = project.id === activeProjectId;
              const subId = `rail-project-${project.id}`;
              return (
                <div key={project.id} className="rail__treenode">
                  <div className="rail__treerow">
                    <Link
                      href={`/projects/${encodeURIComponent(project.id)}`}
                      className="rail__link rail__link--haschild"
                      data-active={projectOnPath || undefined}
                      title={project.id}
                    >
                      <span className="rail__label">{project.name}</span>
                    </Link>
                    <button
                      type="button"
                      className="rail__disclosure"
                      aria-expanded={isOpen}
                      aria-controls={subId}
                      aria-label={isOpen ? `Collapse ${project.name}` : `Expand ${project.name}`}
                      data-testid={`project-toggle-${project.name}`}
                      onClick={() => toggleProject(project.id)}
                    >
                      <Caret open={isOpen} />
                    </button>
                  </div>

                  {isOpen && (
                    <div
                      className="rail__sub rail__sub--sections"
                      id={subId}
                      role="group"
                      aria-label={`${project.name} sections`}
                    >
                      {projectSubnav(project.id).map((s) => {
                        const sectionActive = projectOnPath && activeSection === s.id;
                        return (
                          <Link
                            key={s.id}
                            href={s.href}
                            className="rail__link rail__link--leaf"
                            data-active={sectionActive || undefined}
                            aria-current={sectionActive ? "page" : undefined}
                          >
                            <span className="rail__icon">
                              <NavIcon id={s.id} size={16} />
                            </span>
                            <span className="rail__label">{s.label}</span>
                          </Link>
                        );
                      })}
                    </div>
                  )}
                </div>
              );
            })}
        </div>
      )}
    </div>
  );
}

/** A rotation-only disclosure caret. aria-hidden — the accessible name is on the button (NFR-4). */
function Caret({ open }: { open: boolean }) {
  return (
    <svg
      className="rail__caret"
      data-open={open || undefined}
      width="12"
      height="12"
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="2.5"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
    >
      <path d="M9 6l6 6-6 6" />
    </svg>
  );
}
