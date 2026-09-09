"use client";

// components/projects/ProjectWorkspaceMenu.tsx — the Project-Detail Workspace left menu
// (ISI-3957 S1, AC1/AC3). The operator's "control room" nav down the left edge of every
// /projects/{projectId}/* route: Landing · Issues · Runs · Discussion · File Explorer · GitHub.
//
// URL is the state (nav.ts doctrine): the active entry is a pure derivation of the pathname via
// projectMenuActiveId — no client store decides "which tab". The ONLY client state here is the
// cosmetic collapse/expand, persisted per-user in localStorage (mirrors TicketsScreen's ?view=
// + localStorage split). Collapsing only hides the LABELS (icon-only rail); it never hides the
// active section's content, which lives in the sibling content region (AC1).

import { useEffect, useState } from "react";
import Link from "next/link";
import { usePathname } from "next/navigation";
import { NavIcon } from "@/components/nav/NavIcon";
import { projectMenu, projectMenuActiveId } from "@/lib/nav";

const COLLAPSE_KEY = "ksq:workspace-menu-collapsed";

export function ProjectWorkspaceMenu({ projectId }: { projectId: string }) {
  const pathname = usePathname() ?? "";
  const items = projectMenu(projectId);
  const activeId = projectMenuActiveId(pathname);

  // Cosmetic collapse, persisted per user. SSR-safe: first paint is always expanded (server and
  // client agree), then the effect reads the stored preference after hydration.
  const [collapsed, setCollapsed] = useState(false);
  useEffect(() => {
    try {
      setCollapsed(window.localStorage.getItem(COLLAPSE_KEY) === "1");
    } catch {
      /* storage unavailable (private mode / SSR) — stay expanded */
    }
  }, []);

  function toggle() {
    setCollapsed((prev) => {
      const next = !prev;
      try {
        window.localStorage.setItem(COLLAPSE_KEY, next ? "1" : "0");
      } catch {
        /* ignore */
      }
      return next;
    });
  }

  return (
    <nav
      className="pworkspace__menu"
      data-collapsed={collapsed || undefined}
      aria-label="Project sections"
    >
      <button
        type="button"
        className="pworkspace__collapse"
        aria-label={collapsed ? "Expand project menu" : "Collapse project menu"}
        aria-expanded={!collapsed}
        onClick={toggle}
      >
        <NavIcon id={collapsed ? "menu" : "close"} size={18} />
      </button>
      <ul className="pworkspace__list" role="list">
        {items.map((item) => {
          const active = item.id === activeId;
          return (
            <li key={item.id}>
              <Link
                href={item.href}
                className="pworkspace__link"
                data-active={active || undefined}
                aria-current={active ? "page" : undefined}
                // The label span is display:none when collapsed (icon-only), so the link would
                // otherwise have no accessible name — mirror the visible label here.
                aria-label={item.label}
                title={collapsed ? item.label : undefined}
                data-testid={`pmenu-${item.id}`}
              >
                <span className="pworkspace__icon">
                  <NavIcon id={item.id} />
                </span>
                <span className="pworkspace__label">{item.label}</span>
              </Link>
            </li>
          );
        })}
      </ul>
    </nav>
  );
}
