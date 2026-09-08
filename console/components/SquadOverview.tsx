"use client";

// components/SquadOverview.tsx — the story 8.1 screen body (ISI-2900).
//
// The Team→Project→Run-status projection GET /api/squad/overview answers, rendered at the
// console root. The browser fetches the BFF route (app/api/squad/overview), which proxies the
// Go apiserver read model (overview.go) through the §13 choke point — the caller's session
// resolves server-side to the ONE Team whose projection this is (§7.3.3 tenancy root), so the
// screen never receives (and never asks for) a Team selector: cross-Team data is absent by
// construction, not filtered client-side.
//
// Every terminal HTTP state the BFF can relay gets a distinct, honest rendering:
//   401 → unauthenticated (no session) · 404 → session's Team has no projection yet ·
//   501 → the read model is not wired in this deployment (dev run) · 5xx → retryable error.

import Link from "next/link";
import { useEffect, useState } from "react";
import { KillRun } from "@/components/KillRun";
import { EmptyState } from "@/components/forms/EmptyState";

/** GET /api/squad/overview response (apiserver SquadOverview, overview.go).
 *
 * `projects` and `runs` arrive NULLABLE on the wire: Go marshals a nil slice as `null`, and
 * overview.go builds both with `append`/map-lookup (nil for "none"), with no `omitempty` and no
 * `[]` normalization. Treat them as `| null` everywhere — the normal empty state must render the
 * empty card, not crash the console root. */
export interface ProjectOverview {
  name: string;
  namespace: string;
  repoUrl?: string;
  runs: {
    name: string;
    workItem?: string;
    phase: string;
    claimedAt?: string | null;
  }[] | null;
  phaseCounts: Record<string, number>;
}

export interface SquadOverviewData {
  team: { name: string; namespace: string; uid: string };
  projects: ProjectOverview[] | null;
  /** Set only for a global-admin caller (ISI-3932): the projection then spans every squad and
   * `team` is a synthetic fleet marker (name "*"). Absent/false for a tenant caller, whose
   * projection is their single Team. Branch the render on THIS wire flag, never on session role,
   * so a pre-ISI-3932 apiserver (no `fleet`) degrades safely to the single-Team path. */
  fleet?: boolean;
}

type LoadState =
  | { kind: "loading" }
  | { kind: "unauthenticated" }
  | { kind: "no-team" }
  | { kind: "not-wired" }
  | { kind: "error"; status: number }
  | { kind: "ready"; data: SquadOverviewData };

/** Map an HTTP status to the distinct honest state it carries (see component doc). */
export function classifyOverviewStatus(status: number): LoadState {
  switch (status) {
    case 401:
      return { kind: "unauthenticated" };
    case 404:
      return { kind: "no-team" };
    case 501:
      return { kind: "not-wired" };
    default:
      return { kind: "error", status };
  }
}

/** The status hue a Run phase renders with (token roles from story 8.9). */
export function phaseTone(phase: string): string {
  const p = phase.toLowerCase();
  if (["running", "claiming", "dispatching", "collecting"].some((s) => p.includes(s))) {
    return "running";
  }
  if (p.includes("paused")) return "paused";
  if (["failed", "canceled", "canceling"].some((s) => p.includes(s))) return "blocked";
  return "idle";
}

/** One Project card: phase-count chips + Run table (or a "No Runs." row). Shared verbatim by the
 * tenant and fleet branches so the single-Team render stays byte-for-byte (AC4) while the fleet
 * branch groups these by squad. The React key is applied by the CALLER (namespace-qualified — AC3). */
function ProjectSection({ project: p }: { project: ProjectOverview }) {
  return (
    <section className="card" data-testid="overview-project">
      <h2 style={{ margin: "0 0 4px" }}>
        {/* S6 (ISI-3960): the card title is the entry point into the S1 workspace.
            Link the TITLE only — the card also holds /runs/{id} row anchors, so wrapping
            the whole card would create invalid nested anchors and steal the run clicks.
            Route param is the project CR name; page.tsx decodeURIComponent's it, so encode
            here for symmetry. TODO(ISI-3967): the fleet branch renders same-named projects
            from different squads, so a bare name is ambiguous there; ISI-3967 retargets these
            rows to a namespace-qualified /projects/{ns}/{name} route once it lands. */}
        <Link
          href={`/projects/${encodeURIComponent(p.name)}`}
          data-testid="overview-project-link"
        >
          {p.name}
        </Link>
      </h2>
      {p.repoUrl ? (
        <p className="muted" style={{ margin: "0 0 8px", fontSize: 13 }}>
          <code>{p.repoUrl}</code>
        </p>
      ) : null}
      <div style={{ display: "flex", gap: 8, flexWrap: "wrap", marginBottom: 10 }}>
        {Object.entries(p.phaseCounts ?? {}).map(([phase, n]) => (
          <span
            key={phase}
            className="phase-chip"
            data-tone={phaseTone(phase)}
            data-testid="overview-phase-count"
          >
            {phase} · {n}
          </span>
        ))}
      </div>
      {(p.runs ?? []).length === 0 ? (
        <p className="muted" style={{ margin: 0 }}>
          No Runs.
        </p>
      ) : (
        <table style={{ width: "100%", borderCollapse: "collapse" }}>
          <thead>
            <tr className="muted" style={{ textAlign: "left", fontSize: 12 }}>
              <th style={{ padding: "4px 8px 4px 0" }}>Run</th>
              <th style={{ padding: "4px 8px 4px 0" }}>Work item</th>
              <th style={{ padding: "4px 8px 4px 0" }}>Phase</th>
              <th style={{ padding: "4px 8px 4px 0" }}>Claimed</th>
              <th style={{ padding: "4px 8px 4px 0" }}>Actions</th>
            </tr>
          </thead>
          <tbody>
            {(p.runs ?? []).map((r) => (
              <tr key={r.name} data-testid="overview-run-row">
                <td style={{ padding: "4px 8px 4px 0" }}>
                  <a
                    href={`/runs/${encodeURIComponent(r.name)}${
                      r.workItem ? `?wi=${encodeURIComponent(r.workItem)}` : ""
                    }`}
                  >
                    {r.name}
                  </a>
                </td>
                <td style={{ padding: "4px 8px 4px 0" }}>
                  {r.workItem ? <code>{r.workItem}</code> : <span className="muted">—</span>}
                </td>
                <td style={{ padding: "4px 8px 4px 0" }}>
                  <span className="phase-chip" data-tone={phaseTone(r.phase)}>
                    {r.phase}
                  </span>
                </td>
                <td style={{ padding: "4px 8px 4px 0" }}>
                  {r.claimedAt ? (
                    new Date(r.claimedAt).toLocaleString()
                  ) : (
                    <span className="muted">—</span>
                  )}
                </td>
                <td style={{ padding: "4px 8px 4px 0" }}>
                  <KillRun workItem={r.workItem ?? ""} phase={r.phase} />
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </section>
  );
}

/** The tenant (single-Team) Overview body — unchanged from ISI-2900 (AC4). Renders the Team header,
 * the "Create a project" empty CTA, and each Project card keyed namespace-qualified (AC3). */
function TenantOverview({ data }: { data: SquadOverviewData }) {
  const { team, projects: teamProjects } = data;
  // Normalize the wire's null-for-empty to the arrays the render below assumes.
  const projects = teamProjects ?? [];
  return (
    <div data-testid="overview-ready">
      <header className="card">
        <h1 style={{ margin: 0 }} data-testid="overview-team">
          {team.name}
        </h1>
        <p className="muted" style={{ margin: "6px 0 0" }}>
          Squad overview — Team <code>{team.namespace}/{team.name}</code>: Projects and Run
          status at a glance, no <code>kubectl</code>.
        </p>
      </header>

      {projects.length === 0 ? (
        <EmptyState
          testId="overview-empty"
          title="No Projects yet"
          why="No Projects in this Team's namespace yet."
          ctaLabel="Create a project"
          onCta={() => (window.location.href = "/compose?kind=project")}
        />
      ) : (
        projects.map((p) => (
          <ProjectSection key={`${p.namespace}/${p.name}`} project={p} />
        ))
      )}
    </div>
  );
}

/** The fleet-wide (global-admin) Overview body — ISI-3965 / ISI-3950 S1. The backend already sends
 * every squad's Projects (`fleet:true`, synthetic Team "*"); render them honestly: a fleet header
 * (never leaking "*" or an empty namespace as a Team name — AC1), grouped and labelled by squad
 * (namespace — AC2), keyed namespace-qualified so same-named projects in different squads never
 * collide (AC3), with a fleet-appropriate empty state (AC5). */
function FleetOverview({ data }: { data: SquadOverviewData }) {
  // Normalize the nullable wire slice (Go nil → null) and group by squad (namespace). Sort squads
  // by namespace and projects by name for a deterministic order (matches backend sort discipline).
  const projects = data.projects ?? [];
  const bySquad = new Map<string, ProjectOverview[]>();
  for (const p of projects) {
    const group = bySquad.get(p.namespace) ?? [];
    group.push(p);
    bySquad.set(p.namespace, group);
  }
  const squads = [...bySquad.keys()].sort();

  return (
    <div data-testid="overview-ready">
      <header className="card">
        <h1 style={{ margin: 0 }} data-testid="overview-fleet">
          Fleet overview — all squads
        </h1>
        <p className="muted" style={{ margin: "6px 0 0" }}>
          Every squad&apos;s Projects and Run status at a glance, no <code>kubectl</code>.
        </p>
      </header>

      {squads.length === 0 ? (
        <EmptyState
          testId="overview-fleet-empty"
          title="No Projects in any squad yet"
          why="No squad across the fleet has a Project yet."
        />
      ) : (
        squads.map((ns) => (
          <section key={ns} data-testid="overview-squad-group">
            <h2 className="muted" style={{ margin: "18px 0 6px" }} data-testid="overview-squad-label">
              Squad <code>{ns}</code>
            </h2>
            {(bySquad.get(ns) ?? [])
              .slice()
              .sort((a, b) => a.name.localeCompare(b.name))
              .map((p) => (
                <ProjectSection key={`${p.namespace}/${p.name}`} project={p} />
              ))}
          </section>
        ))
      )}
    </div>
  );
}

export function SquadOverview() {
  const [state, setState] = useState<LoadState>({ kind: "loading" });

  useEffect(() => {
    let alive = true;
    fetch("/api/squad/overview", { headers: { accept: "application/json" } })
      .then(async (res) => {
        if (!alive) return;
        if (!res.ok) {
          setState(classifyOverviewStatus(res.status));
          return;
        }
        setState({ kind: "ready", data: (await res.json()) as SquadOverviewData });
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
      <div className="card" data-testid="overview-loading">
        Loading squad overview…
      </div>
    );
  }
  if (state.kind === "unauthenticated") {
    return (
      <div className="card" data-testid="overview-unauthenticated">
        <h2 style={{ marginTop: 0 }}>Sign in required</h2>
        <p className="muted" style={{ marginBottom: 0 }}>
          The squad overview is scoped to your session&apos;s Team. Authenticate through the
          console sign-in flow and reload.
        </p>
      </div>
    );
  }
  if (state.kind === "no-team") {
    return (
      <div className="card" data-testid="overview-no-team">
        <h2 style={{ marginTop: 0 }}>No squad for your Team yet</h2>
        <p className="muted" style={{ marginBottom: 0 }}>
          Your session resolves to a Team with no projection — the Team may be newly created
          (or deleted) and the cache has not observed it yet.
        </p>
      </div>
    );
  }
  if (state.kind === "not-wired") {
    return (
      <div className="card" data-testid="overview-not-wired">
        <h2 style={{ marginTop: 0 }}>Squad overview not wired</h2>
        <p className="muted" style={{ marginBottom: 0 }}>
          This apiserver runs without the squad-overview read model (dev / cluster-less run)
          and answers its documented 501.
        </p>
      </div>
    );
  }
  if (state.kind === "error") {
    return (
      <div className="card" data-testid="overview-error">
        <h2 style={{ marginTop: 0 }}>Squad overview unavailable</h2>
        <p className="muted" style={{ marginBottom: 0 }}>
          The read model could not be reached (HTTP {state.status || "network error"}).
          Retry shortly.
        </p>
      </div>
    );
  }

  // Branch on the WIRE flag, never on session role (AC4): a pre-ISI-3932 apiserver sends no
  // `fleet`, so an admin on a stale deployment safely renders the single-Team tenant path.
  return state.data.fleet === true ? (
    <FleetOverview data={state.data} />
  ) : (
    <TenantOverview data={state.data} />
  );
}
