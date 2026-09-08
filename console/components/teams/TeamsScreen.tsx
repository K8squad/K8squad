"use client";

// TeamsScreen — the Teams LIST surface (ISI-3953, gap G4 of the ISI-3949
// fleet-admin audit).
//
// A pure consumer of the apiserver's Teams read model behind the BFF choke
// point (GET /api/teams). A global admin sees every squad's Team (fleet); a
// tenant sees exactly their own Team (existence-hiding — the apiserver never
// leaks another Team). This surface replaces the old non-fetching stub and is
// the enumeration source the fleet Team picker (ISI-3950, gap G1) will consume.
//
// Honesty rules mirror the Credentials screen: the documented 501 (read model
// not wired) renders an explicit "not configured" state; a deny/absence
// (401/403/404) renders "not found"; an authenticated caller with no Teams
// renders a neutral empty state — never a fabricated row.

import { useEffect, useState } from "react";
import {
  classifyTeamsStatus,
  type TeamSummary,
  type TeamsOutcomeKind,
} from "@/lib/teams";
import { EmptyState } from "@/components/forms/EmptyState";

export interface TeamsScreenProps {
  /** Loader for the Teams list (BFF GET /api/teams). Injectable for tests. */
  load?: () => Promise<Response>;
}

export function TeamsScreen({ load = defaultLoad }: TeamsScreenProps) {
  const [state, setState] = useState<TeamsOutcomeKind | "loading">("loading");
  const [teams, setTeams] = useState<TeamSummary[]>([]);
  const [fleet, setFleet] = useState(false);

  useEffect(() => {
    let alive = true;
    load()
      .then((res) => {
        if (!alive) return;
        if (res.status === 501) {
          setState("unconfigured");
          return;
        }
        if (res.status >= 200 && res.status < 300) {
          return res.json().then((body) => {
            if (!alive) return;
            const rows: TeamSummary[] = Array.isArray(body?.teams) ? body.teams : [];
            setTeams(rows);
            setFleet(Boolean(body?.fleet));
            setState(rows.length === 0 ? "empty" : "ok");
          });
        }
        setState(classifyTeamsStatus(res.status));
      })
      .catch(() => alive && setState("error"));
    return () => {
      alive = false;
    };
  }, [load]);

  return (
    <section className="teams" data-testid="teams-screen" data-state={state}>
      <header className="teams__head">
        <h1>Teams</h1>
        <p className="muted">
          {fleet
            ? "Every squad in the fleet — you are viewing as an admin."
            : "The squads you can see."}
        </p>
      </header>

      {state === "loading" && (
        <p className="muted" data-testid="teams-loading">
          Loading teams…
        </p>
      )}

      {state === "unconfigured" && (
        <EmptyState
          testId="teams-unconfigured"
          title="Teams read model not configured"
          why="The apiserver answers its documented 501 — no Teams read model is wired on this host (cluster-less run). The list lights up when the informer cache backs GET /api/teams."
        />
      )}

      {state === "not-found" && (
        <EmptyState
          testId="teams-not-found"
          title="No Teams surface for this session"
          why="Sign in with a squad-scoped session — deny and missing are indistinguishable here by design."
        />
      )}

      {state === "error" && (
        <EmptyState
          testId="teams-error"
          title="Teams unavailable"
          why="The apiserver could not serve the read model — retry shortly."
        />
      )}

      {state === "empty" && (
        <EmptyState
          testId="teams-empty"
          title="No teams yet"
          why="No Team is visible to this session yet. Compose a squad to create the first Team."
        />
      )}

      {state === "ok" && (
        <div className="teams__table-wrap" data-testid="teams-table">
          <table className="teams__table">
            <thead>
              <tr>
                <th>Team</th>
                <th>Namespace</th>
                <th>Org</th>
              </tr>
            </thead>
            <tbody>
              {teams.map((t) => (
                <tr key={t.uid} data-team={t.uid} data-namespace={t.namespace}>
                  <td className="teams__name">{t.name}</td>
                  <td>
                    <code className="teams__ns">{t.namespace}</code>
                  </td>
                  <td>
                    <a
                      className="teams__org-link"
                      href={`/agents?team=${encodeURIComponent(t.uid)}`}
                    >
                      View org
                    </a>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  );
}

async function defaultLoad(): Promise<Response> {
  return fetch("/api/teams", { cache: "no-store" });
}
