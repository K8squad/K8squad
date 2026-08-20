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

import { useEffect, useState } from "react";

/** GET /api/squad/overview response (apiserver SquadOverview, overview.go).
 *
 * `projects` and `runs` arrive NULLABLE on the wire: Go marshals a nil slice as `null`, and
 * overview.go builds both with `append`/map-lookup (nil for "none"), with no `omitempty` and no
 * `[]` normalization. Treat them as `| null` everywhere — the normal empty state must render the
 * empty card, not crash the console root. */
export interface SquadOverviewData {
  team: { name: string; namespace: string; uid: string };
  projects: {
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
  }[] | null;
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

  const { team, projects: teamProjects } = state.data;
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
          status at a glance, no <code>kubectl</code> (story 8.1).
        </p>
      </header>

      {projects.length === 0 ? (
        <div className="card" data-testid="overview-empty">
          <p className="muted" style={{ margin: 0 }}>
            No Projects in this Team&apos;s namespace yet.
          </p>
        </div>
      ) : (
        projects.map((p) => (
          <section className="card" key={p.name} data-testid="overview-project">
            <h2 style={{ margin: "0 0 4px" }}>{p.name}</h2>
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
                  </tr>
                </thead>
                <tbody>
                  {(p.runs ?? []).map((r) => (
                    <tr key={r.name} data-testid="overview-run-row">
                      <td style={{ padding: "4px 8px 4px 0" }}>
                        <a href={`/runs/${encodeURIComponent(r.name)}`}>{r.name}</a>
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
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </section>
        ))
      )}
    </div>
  );
}
