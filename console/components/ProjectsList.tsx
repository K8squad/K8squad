"use client";

// components/ProjectsList.tsx — the Projects tab body (ISI-3943, follow-up to ISI-3941).
//
// The Projects nav destination used to be a placeholder that punted to Overview (ISI-3941 root
// cause: the apiserver had no GET list route for the tab to call). This renders the dedicated
// fleet-aware list the new GET /api/squad/projects route answers, proxied by the BFF
// (app/api/squad/projects). Scoping is authoritative in the apiserver — admin ⇒ fleet-wide
// (ADR-0010), tenant ⇒ their Team namespace — so this screen never asks for or receives a Team
// selector; cross-Team data is absent by construction, not filtered client-side.
//
// Each row deep-links two ways: the PRIMARY click (S6/ISI-3967 AC2 — was the interim
// /agents?team= jump while the workspace didn't exist) opens the project's S1 workspace at
// /projects/{namespace/name} — the same {namespace}/{name} id ProjectSelector uses, and the
// strongest fleet-wide qualifier available pre-ISI-3941. The sub-nav there reaches tickets,
// runs et al. Secondarily, when the owning Team's UID resolved, the row keeps the jump into
// that Team's agents org (/agents?team={teamUid}, ISI-3943 AC2) so an admin can hop from a
// fleet project straight to its squad's agents. Every terminal HTTP state the BFF relays gets
// a distinct honest rendering, mirroring SquadOverview.

import { useEffect, useState } from "react";
import Link from "next/link";
import { EmptyState } from "@/components/forms/EmptyState";
import { classifyOverviewStatus, phaseTone } from "@/components/SquadOverview";

/** GET /api/squad/projects response (apiserver SquadProjectList, overview.go).
 *
 * `projects` arrives non-null on the wire (the apiserver initializes it to an empty slice), but we
 * still treat it defensively as `| null` so an older apiserver that marshals nil-as-null renders
 * the empty state rather than crashing. */
export interface ProjectsListData {
  projects:
    | {
        name: string;
        namespace: string;
        teamUid?: string;
        teamName?: string;
        repoUrl?: string;
        phaseCounts: Record<string, number>;
      }[]
    | null;
  fleet?: boolean;
}

type LoadState =
  | { kind: "loading" }
  | { kind: "unauthenticated" }
  | { kind: "no-team" }
  | { kind: "not-wired" }
  | { kind: "error"; status: number }
  | { kind: "ready"; data: ProjectsListData };

/** The {namespace}/{name} id the project sub-nav routes on (matches ProjectSelector). */
export function projectId(namespace: string, name: string): string {
  return namespace ? `${namespace}/${name}` : name;
}

export function ProjectsList() {
  const [state, setState] = useState<LoadState>({ kind: "loading" });

  useEffect(() => {
    let alive = true;
    fetch("/api/squad/projects", { headers: { accept: "application/json" } })
      .then(async (res) => {
        if (!alive) return;
        if (!res.ok) {
          const classified = classifyOverviewStatus(res.status);
          setState(classified as LoadState);
          return;
        }
        setState({ kind: "ready", data: (await res.json()) as ProjectsListData });
      })
      .catch(() => {
        if (alive) setState({ kind: "error", status: 0 });
      });
    return () => {
      alive = false;
    };
  }, []);

  if (state.kind === "loading") {
    return (
      <div className="card" data-testid="projects-loading">
        Loading projects…
      </div>
    );
  }
  if (state.kind === "unauthenticated") {
    return (
      <div className="card" data-testid="projects-unauthenticated">
        <h2 style={{ marginTop: 0 }}>Sign in required</h2>
        <p className="muted" style={{ marginBottom: 0 }}>
          The Projects list is scoped to your session. Authenticate through the console sign-in
          flow and reload.
        </p>
      </div>
    );
  }
  if (state.kind === "no-team") {
    return (
      <div className="card" data-testid="projects-no-team">
        <h2 style={{ marginTop: 0 }}>No squad for your Team yet</h2>
        <p className="muted" style={{ marginBottom: 0 }}>
          Your session resolves to a Team with no projection — the Team may be newly created (or
          deleted) and the cache has not observed it yet.
        </p>
      </div>
    );
  }
  if (state.kind === "not-wired") {
    return (
      <div className="card" data-testid="projects-not-wired">
        <h2 style={{ marginTop: 0 }}>Projects list not wired</h2>
        <p className="muted" style={{ marginBottom: 0 }}>
          This apiserver runs without the projects read model (dev / cluster-less run) and answers
          its documented 501.
        </p>
      </div>
    );
  }
  if (state.kind === "error") {
    return (
      <div className="card" data-testid="projects-error">
        <h2 style={{ marginTop: 0 }}>Projects unavailable</h2>
        <p className="muted" style={{ marginBottom: 0 }}>
          The read model could not be reached (HTTP {state.status || "network error"}). Retry
          shortly.
        </p>
      </div>
    );
  }

  const projects = state.data.projects ?? [];
  return (
    <div data-testid="projects-ready">
      <header className="card">
        <h1 style={{ margin: 0 }}>Projects</h1>
        <p className="muted" style={{ margin: "6px 0 0" }}>
          {state.data.fleet
            ? "Fleet-wide — every squad's projects across the cluster."
            : "Your squad's projects. Open one to reach its build, tickets, runs and discussion."}
        </p>
      </header>

      {projects.length === 0 ? (
        <EmptyState
          testId="projects-empty"
          title="No Projects yet"
          why="No Projects in your Team's namespace yet."
          ctaLabel="Create a project"
          onCta={() => (window.location.href = "/compose?kind=project")}
        />
      ) : (
        projects.map((p) => {
          const id = projectId(p.namespace, p.name);
          return (
            <section
              className="card"
              key={id}
              data-testid="projects-row"
              data-team-uid={p.teamUid ?? ""}
            >
              <h2 style={{ margin: "0 0 4px" }}>
                {/* S6 (ISI-3967, AC2 of ISI-3960): the row title is the primary entry into the
                    S1 workspace (Landing). Same encodeURIComponent idiom as SquadOverview
                    (ISI-3960 AC1): the route decodeURIComponent's the param, so encode here for
                    symmetry. The id is namespace-qualified (projectId()) — unlike the
                    team-scoped Overview, this list is fleet-wide, so a bare name could collide
                    across squads. TODO(ISI-3941): if a namespace/team-UID-qualified route lands,
                    build this link from the ProjectListEntry owning Team UID instead — same seam
                    ISI-3960 marks in SquadOverview. */}
                <Link
                  href={`/projects/${encodeURIComponent(id)}`}
                  data-testid="projects-row-link"
                >
                  {p.name}
                </Link>
              </h2>
              <p className="muted" style={{ margin: "0 0 8px", fontSize: 13 }}>
                {state.data.fleet && p.teamName ? (
                  <>
                    Squad <code>{p.teamName}</code> · <code>{p.namespace}</code>
                  </>
                ) : (
                  <code>{p.namespace}</code>
                )}
                {p.repoUrl ? (
                  <>
                    {" · "}
                    <code>{p.repoUrl}</code>
                  </>
                ) : null}
              </p>
              <div style={{ display: "flex", gap: 8, flexWrap: "wrap", marginBottom: 8 }}>
                {Object.entries(p.phaseCounts ?? {}).map(([phase, n]) => (
                  <span
                    key={phase}
                    className="phase-chip"
                    data-tone={phaseTone(phase)}
                    data-testid="projects-phase-count"
                  >
                    {phase} · {n}
                  </span>
                ))}
              </div>
              {p.teamUid ? (
                <Link
                  href={`/agents?team=${encodeURIComponent(p.teamUid)}`}
                  className="muted"
                  style={{ fontSize: 13 }}
                  data-testid="projects-agents-link"
                >
                  View squad agents →
                </Link>
              ) : null}
            </section>
          );
        })
      )}
    </div>
  );
}
