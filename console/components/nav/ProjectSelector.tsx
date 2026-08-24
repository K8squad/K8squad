"use client";

// components/nav/ProjectSelector.tsx — the Project context selector (story 8.13).
//
// Sits at the top of the rail under the Project root. Selecting a project navigates to that
// project's sub-nav root (`/projects/{id}/tickets`) — the URL is the active-project state; no
// client store. Options come from the BFF (`/api/projects`, which projects the apiserver's
// squad-overview read model into a project list, §13 choke point); a BFF failure degrades to an
// empty state rather than a broken control — the selector still shows the in-URL project so
// deep links never lose their context.

import { useEffect, useState } from "react";
import { useRouter } from "next/navigation";
import { NavIcon } from "@/components/nav/NavIcon";

export type ProjectOption = { id: string; name: string };

export function ProjectSelector({
  projects,
  activeId,
  onNavigate,
}: {
  projects: ProjectOption[];
  activeId: string | null;
  onNavigate: (projectId: string) => void;
}) {
  const active =
    projects.find((p) => p.id === activeId) ??
    (activeId ? { id: activeId, name: activeId } : null);
  return (
    <label className="projselect" data-empty={projects.length === 0}>
      <span className="projselect__caption">Project context</span>
      <span className="projselect__control">
        <NavIcon id="project" size={16} />
        <select
          className="projselect__select"
          value={active?.id ?? ""}
          onChange={(e) => {
            if (e.target.value) onNavigate(e.target.value);
          }}
          aria-label="Select project context"
        >
          {active && !projects.some((p) => p.id === active.id) && (
            <option value={active.id}>{active.name}</option>
          )}
          {projects.map((p) => (
            <option key={p.id} value={p.id}>
              {p.name}
            </option>
          ))}
          {projects.length === 0 && !active && (
            <option value="">No projects</option>
          )}
        </select>
      </span>
    </label>
  );
}

/** Client fetch of the project list via the BFF (graceful empty state on failure). */
export function useProjects(): ProjectOption[] {
  const [projects, setProjects] = useState<ProjectOption[]>([]);
  useEffect(() => {
    let alive = true;
    fetch("/api/projects", { cache: "no-store" })
      .then((r) => (r.ok ? r.json() : Promise.reject(new Error(String(r.status)))))
      .then((body: { projects?: ProjectOption[] }) => {
        if (alive && Array.isArray(body.projects)) setProjects(body.projects);
      })
      .catch(() => {
        /* degrade: empty selector; URL context still renders */
      });
    return () => {
      alive = false;
    };
  }, []);
  return projects;
}

export function NavigatingProjectSelector({ activeId }: { activeId: string | null }) {
  const projects = useProjects();
  const router = useRouter();
  return (
    <ProjectSelector
      projects={projects}
      activeId={activeId}
      onNavigate={(id) => router.push(`/projects/${encodeURIComponent(id)}/tickets`)}
    />
  );
}
