"use client";

// components/agents/FleetTeamPicker.tsx — the fleet team picker for a global admin (ISI-3964,
// Phase-2 of ISI-3941).
//
// A tenant session resolves to exactly ONE Team, so the Agents landing has no team selector for
// them (see SessionTeamOrg — that path is unchanged). A global admin, however, has no home tenancy
// (the bootstrap admin's team_id backs no Team CR, ISI-3921) so SessionTeamOrg resolves the fleet
// overview's synthetic `Team:"*"` (empty uid) to "no team" — the headline dead-end this fixes.
//
// This picker fetches the fleet-aware GET /api/squad/teams (fleetlist.go FleetTeamList — admin ⇒
// every squad) and lists each Team as a card that deep-links to `/agents?team={uid}`, the existing
// override the Agents page already honors (→ TeamOrgDiagram, itself fleet-aware). Read-only; the
// apiserver owns the authZ scope (this only renders whatever squads the session is entitled to see).

import Link from "next/link";
import { useEffect, useState } from "react";

/** One Team row of GET /api/squad/teams (apiserver TeamListEntry, fleetlist.go). */
export interface FleetTeamEntry {
  name: string;
  namespace: string;
  uid: string;
  agentCount: number;
  projectCount: number;
}

/** GET /api/squad/teams response (apiserver FleetTeamList). `teams` may arrive `null`/absent. */
export interface FleetTeamListData {
  teams: FleetTeamEntry[] | null;
  fleet?: boolean;
}

type LoadState =
  | { kind: "loading" }
  | { kind: "unauthenticated" }
  | { kind: "not-wired" }
  | { kind: "error"; status: number }
  | { kind: "ready"; teams: FleetTeamEntry[]; fleet: boolean };

/** Map an HTTP status + parsed body to the honest state it carries. Pure — unit-testable. */
export function classifyTeamList(
  status: number,
  body: FleetTeamListData | null,
): LoadState {
  if (status === 401) return { kind: "unauthenticated" };
  if (status === 501) return { kind: "not-wired" };
  if (status < 200 || status >= 300) return { kind: "error", status };
  return {
    kind: "ready",
    teams: body?.teams ?? [],
    fleet: body?.fleet ?? false,
  };
}

/** Fetch the fleet Team list and render a squad picker. Admin-only surface. */
export function FleetTeamPicker() {
  const [state, setState] = useState<LoadState>({ kind: "loading" });

  useEffect(() => {
    let alive = true;
    fetch("/api/squad/teams", {
      headers: { accept: "application/json" },
      cache: "no-store",
    })
      .then(async (res) => {
        if (!alive) return;
        const body = res.ok
          ? ((await res.json()) as FleetTeamListData)
          : null;
        if (alive) setState(classifyTeamList(res.status, body));
      })
      .catch(() => {
        if (alive) setState({ kind: "error", status: 0 });
      });
    return () => {
      alive = false;
    };
  }, []);

  if (state.kind === "loading") {
    return <p className="muted">Loading squads…</p>;
  }
  if (state.kind === "unauthenticated") {
    return (
      <p className="muted">
        Sign in through the console sign-in flow to browse the fleet.
      </p>
    );
  }
  if (state.kind === "not-wired") {
    return (
      <p className="muted" data-testid="fleet-teams-not-wired">
        The fleet team list isn’t available in this deployment yet.
      </p>
    );
  }
  if (state.kind === "error") {
    return (
      <p className="muted">Couldn’t load the fleet team list. Try again.</p>
    );
  }
  if (state.teams.length === 0) {
    return (
      <p className="muted" data-testid="fleet-teams-empty">
        No squads exist in the fleet yet.
      </p>
    );
  }

  return (
    <div data-testid="fleet-team-picker">
      <p className="muted">
        Pick a squad to view its org chart — Team → Agent → Role.
      </p>
      <ul className="fleet-team-picker__list">
        {state.teams.map((t) => (
          <li key={t.uid || `${t.namespace}/${t.name}`}>
            <Link
              className="card fleet-team-picker__item"
              href={`/agents?team=${encodeURIComponent(t.uid)}`}
              data-testid="fleet-team-link"
            >
              <span className="fleet-team-picker__name">{t.name}</span>
              <span className="fleet-team-picker__meta muted">
                {t.namespace} · {t.agentCount} agent
                {t.agentCount === 1 ? "" : "s"} · {t.projectCount} project
                {t.projectCount === 1 ? "" : "s"}
              </span>
            </Link>
          </li>
        ))}
      </ul>
    </div>
  );
}
