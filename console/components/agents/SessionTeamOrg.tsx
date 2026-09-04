"use client";

// components/agents/SessionTeamOrg.tsx — resolves the session's Team and renders its org diagram
// (Agents landing, fix ISI-3543).
//
// The Agents landing has NO manual team selector BY DESIGN: a session resolves to exactly ONE Team
// by UID (§7.3.3 tenancy root — see SquadOverview) so cross-Team browsing is absent by construction,
// not filtered client-side. A dropdown "select a team" would contradict the tenancy model. Instead
// this reuses the already-wired GET /api/squad/overview to resolve that one Team's UID (the value
// AuthorContext.TeamID carries, overview.go) and hands it to <TeamOrgDiagram>. Every terminal state
// of the resolve renders an honest surface (unauthenticated / no-team / error) — never scaffold or
// internal story-reference copy.

import { useEffect, useState } from "react";
import { TeamOrgDiagram } from "./TeamOrgDiagram";

export type ResolveState =
  | { kind: "loading" }
  | { kind: "unauthenticated" }
  | { kind: "no-team" }
  | { kind: "error" }
  | { kind: "ok"; teamId: string };

/**
 * Map an overview response (status + parsed body) to the resolve state. Mirrors SquadOverview's
 * honest status map: 401 unauthenticated · 404 no team for this session · 501/5xx retryable. The
 * org read-model is Team-scoped, so a session with no team (404, or a 200 body carrying no
 * `team.uid`) has no org to show — existence-hiding, not an error. Pure so it is unit-testable.
 */
export function resolveTeamState(
  status: number,
  body: { team?: { uid?: string } } | null,
): ResolveState {
  if (status === 401) return { kind: "unauthenticated" };
  if (status === 404) return { kind: "no-team" };
  if (status < 200 || status >= 300) return { kind: "error" };
  const uid = body?.team?.uid;
  if (!uid) return { kind: "no-team" };
  return { kind: "ok", teamId: uid };
}

/** Resolve the session's Team UID from the squad-overview projection, then render its org diagram. */
export function SessionTeamOrg() {
  const [state, setState] = useState<ResolveState>({ kind: "loading" });

  useEffect(() => {
    let alive = true;
    fetch("/api/squad/overview", {
      headers: { accept: "application/json" },
      cache: "no-store",
    })
      .then(async (res) => {
        if (!alive) return;
        const body = res.ok
          ? ((await res.json()) as { team?: { uid?: string } })
          : null;
        if (alive) setState(resolveTeamState(res.status, body));
      })
      .catch(() => {
        if (alive) setState({ kind: "error" });
      });
    return () => {
      alive = false;
    };
  }, []);

  if (state.kind === "loading") {
    return <p className="muted">Resolving your team…</p>;
  }
  if (state.kind === "unauthenticated") {
    return (
      <p className="muted">
        Sign in through the console sign-in flow to view your team’s agents.
      </p>
    );
  }
  if (state.kind === "no-team") {
    // Reached on 404 or a 200 body with no `team.uid` — the session has no Team org projection to
    // resolve. This is NOT "a Team with zero agents": a real empty Team resolves to `ok` and
    // TeamOrgDiagram renders its own empty-agents state. So the copy speaks to the missing
    // projection, not agent count.
    return (
      <p className="muted">
        No team org chart is available for your session yet.
      </p>
    );
  }
  if (state.kind === "error") {
    return <p className="muted">Couldn’t resolve your team. Try again.</p>;
  }

  return <TeamOrgDiagram teamId={state.teamId} />;
}
