"use client";

// components/nav/TeamsNavTree.tsx — the Teams rail sub-tree (ISI-4001, story ISI-3995).
//
// The rail's FIRST dynamic, lazy-loaded sub-tree. `navTree()` stays pure and static (lib/nav.ts);
// this scoped client island is mounted by ConsoleShell where the Teams node's accordion would
// render (marker: NavNode.dynamicChildren === "teams"). It expands in two lazy levels:
//   Teams (link → /teams)  →  team(s)  →  that team's agents (each a link → /agents/{id}).
//
// Data is REUSE-ONLY (no backend this story): teams = GET /api/teams (ISI-3953, the lighter
// dependency with a ready TS type + classifier — chosen over /api/squad/teams because the rail
// needs only {name,uid}, not fleet-only fields); per-team agents = GET /api/teams/{uid}/org
// (existing, TeamOrg.agents). Tenant-vs-admin scoping + existence-hiding are enforced SERVER-SIDE
// by those routes — the island renders whatever they return and never synthesizes a team/agent.
//
// Honesty (AC5, FR-I3, cf. ISI-3686): every sub-level owns its outcome machine reusing
// classifyTeamsStatus — loading spinner, neutral empty leaf, 501 "not wired yet", deny/not-found,
// and an error with a retry affordance. A sub-tree failure degrades inline; it never blanks or
// crashes the rest of the rail (ConsoleShell wraps this in an error boundary).
//
// URL-is-state (AC4/AC6): only expand/collapse + fetched children live in client state (top-level
// Teams expansion persists per user via localStorage, mirroring ISI-3957). Which item is ACTIVE is
// a pure derivation of the pathname — the agent leaf whose id is in the URL, its ancestor team, and
// Teams all light up.

import { useCallback, useEffect, useRef, useState } from "react";
import Link from "next/link";
import { usePathname } from "next/navigation";
import { NavIcon } from "@/components/nav/NavIcon";
import { classifyTeamsStatus, type TeamSummary } from "@/lib/teams";
import type { OrgAgent, TeamOrg } from "@/lib/agents/types";

/** Per-sub-level outcome kinds — the same vocabulary as TeamsScreen/CredentialsScreen. */
type SubState =
  | "loading"
  | "ok"
  | "empty"
  | "unconfigured"
  | "not-found"
  | "error";

/** Resolved (cached) agents for one team, keyed by the team uid. */
type OrgEntry = { state: SubState; agents: OrgAgent[] };

const TEAMS_EXPAND_KEY = "ksquad.nav.teams.expanded";

export interface TeamsNavTreeProps {
  /** True when the Teams node is the active nav (pathname → /teams); the URL still owns active. */
  active?: boolean;
  /** Pathname override for tests; defaults to usePathname(). */
  pathname?: string;
  /** Loader for the Teams list (BFF GET /api/teams). Injectable for tests. */
  loadTeams?: () => Promise<Response>;
  /** Loader for one team's org (BFF GET /api/teams/{uid}/org). Injectable for tests. */
  loadOrg?: (uid: string) => Promise<Response>;
  /** Start with Teams expanded (tests / deterministic SSR); overrides the persisted value. */
  defaultExpanded?: boolean;
}

const defaultLoadTeams = () => fetch("/api/teams", { cache: "no-store" });
const defaultLoadOrg = (uid: string) =>
  fetch(`/api/teams/${encodeURIComponent(uid)}/org`, { cache: "no-store" });

/** The active agent id embedded in an /agents/{id} route (null elsewhere). Pure, URL-only. */
function activeAgentId(pathname: string): string | null {
  const m = pathname.match(/^\/agents\/([^/?#]+)/);
  return m ? decodeURIComponent(m[1]) : null;
}

export function TeamsNavTree({
  active = false,
  pathname: pathnameProp,
  loadTeams = defaultLoadTeams,
  loadOrg = defaultLoadOrg,
  defaultExpanded,
}: TeamsNavTreeProps) {
  const routerPath = usePathname();
  const pathname = pathnameProp ?? routerPath ?? "/";
  const agentId = activeAgentId(pathname);

  const [expanded, setExpanded] = useState<boolean>(defaultExpanded ?? false);
  const [teamsState, setTeamsState] = useState<SubState | "unopened">("unopened");
  const [teams, setTeams] = useState<TeamSummary[]>([]);
  const [expandedTeams, setExpandedTeams] = useState<Set<string>>(new Set());
  const [orgs, setOrgs] = useState<Record<string, OrgEntry>>({});
  // Bumped to force a teams refetch on the retry affordance (AC5).
  const [teamsNonce, setTeamsNonce] = useState(0);

  // Restore the persisted top-level expansion once, after mount (SSR-safe; no hydration mismatch
  // since the server always renders collapsed). An explicit defaultExpanded wins (tests).
  useEffect(() => {
    if (defaultExpanded !== undefined) return;
    try {
      if (localStorage.getItem(TEAMS_EXPAND_KEY) === "1") setExpanded(true);
    } catch {
      /* storage blocked — stay collapsed, cosmetic only */
    }
  }, [defaultExpanded]);

  const persistExpanded = useCallback((next: boolean) => {
    try {
      localStorage.setItem(TEAMS_EXPAND_KEY, next ? "1" : "0");
    } catch {
      /* ignore */
    }
  }, []);

  // Fetch the teams list on first expand (and on retry). Lazy: nothing is fetched while collapsed.
  useEffect(() => {
    if (!expanded) return;
    if (teamsState !== "unopened" && teamsNonce === 0) return;
    let alive = true;
    setTeamsState("loading");
    loadTeams()
      .then((res) => {
        if (!alive) return;
        if (res.status >= 200 && res.status < 300) {
          return res.json().then((body: unknown) => {
            if (!alive) return;
            const rows: TeamSummary[] = Array.isArray((body as { teams?: unknown })?.teams)
              ? ((body as { teams: TeamSummary[] }).teams)
              : [];
            // Deterministic order (AC2): by name, then namespace as a stable tiebreaker.
            const sorted = [...rows].sort(
              (a, b) => a.name.localeCompare(b.name) || a.namespace.localeCompare(b.namespace),
            );
            setTeams(sorted);
            setTeamsState(sorted.length === 0 ? "empty" : "ok");
          });
        }
        setTeamsState(classifyTeamsStatus(res.status));
      })
      .catch(() => alive && setTeamsState("error"));
    return () => {
      alive = false;
    };
    // teamsNonce drives retry; teamsState intentionally excluded to avoid a refetch loop.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [expanded, teamsNonce, loadTeams]);

  const fetchOrg = useCallback(
    (uid: string) => {
      let alive = true;
      setOrgs((prev) => ({ ...prev, [uid]: { state: "loading", agents: [] } }));
      loadOrg(uid)
        .then((res) => {
          if (!alive) return;
          if (res.status >= 200 && res.status < 300) {
            return res.json().then((body: unknown) => {
              if (!alive) return;
              const agents: OrgAgent[] = Array.isArray((body as TeamOrg)?.agents)
                ? (body as TeamOrg).agents
                : [];
              const sorted = [...agents].sort((a, b) => a.name.localeCompare(b.name));
              setOrgs((prev) => ({
                ...prev,
                [uid]: { state: sorted.length === 0 ? "empty" : "ok", agents: sorted },
              }));
            });
          }
          setOrgs((prev) => ({
            ...prev,
            [uid]: { state: classifyTeamsStatus(res.status), agents: [] },
          }));
        })
        .catch(() => {
          if (alive) setOrgs((prev) => ({ ...prev, [uid]: { state: "error", agents: [] } }));
        });
      return () => {
        alive = false;
      };
    },
    [loadOrg],
  );

  const toggleTeams = useCallback(() => {
    setExpanded((v) => {
      const next = !v;
      persistExpanded(next);
      return next;
    });
  }, [persistExpanded]);

  const toggleTeam = useCallback(
    (uid: string) => {
      const wasExpanded = expandedTeams.has(uid);
      setExpandedTeams((prev) => {
        const next = new Set(prev);
        if (next.has(uid)) {
          next.delete(uid);
        } else {
          next.add(uid);
        }
        return next;
      });
      // Lazy-load only on first OPEN; cached results (ok/empty) are never refetched (AC3). An
      // errored entry is allowed to refetch on re-open so a transient failure can recover.
      if (!wasExpanded) {
        const existing = orgs[uid];
        if (!existing || existing.state === "error") fetchOrg(uid);
      }
    },
    [expandedTeams, orgs, fetchOrg],
  );

  const retryTeams = useCallback(() => setTeamsNonce((n) => n + 1), []);

  // A team is on the active path when the agent in the URL is one of its (loaded) agents (AC4).
  const activeTeamUid = agentId
    ? teams.find((t) => orgs[t.uid]?.agents.some((a) => a.id === agentId))?.uid ?? null
    : null;
  const teamsActive = active || activeTeamUid !== null;

  return (
    <div className="rail__tree" data-testid="teams-nav-tree">
      {/* Teams row: the label links to /teams; a SEPARATE chevron toggles the sub-tree (AC1) —
          two distinct, keyboard-operable affordances. */}
      <div className="rail__treerow">
        <Link
          href="/teams"
          className="rail__link rail__link--haschild"
          data-active={teamsActive || undefined}
          aria-current={teamsActive ? "page" : undefined}
          aria-label="Teams"
        >
          <span className="rail__icon">
            <NavIcon id="teams" />
          </span>
          <span className="rail__label">Teams</span>
        </Link>
        <button
          type="button"
          className="rail__disclosure"
          aria-expanded={expanded}
          aria-controls="rail-teams-subtree"
          aria-label={expanded ? "Collapse Teams" : "Expand Teams"}
          data-testid="teams-toggle"
          onClick={toggleTeams}
        >
          <Caret open={expanded} />
        </button>
      </div>

      {expanded && (
        <div className="rail__sub" id="rail-teams-subtree" role="group" aria-label="Teams">
          {teamsState === "loading" && (
            <p className="rail__navstate" data-testid="teams-tree-loading" role="status">
              <span className="rail__spinner" aria-hidden="true" />
              Loading teams…
            </p>
          )}

          {teamsState === "unconfigured" && (
            <p className="rail__navstate rail__navstate--muted" data-testid="teams-tree-unconfigured">
              Teams not wired yet
            </p>
          )}

          {teamsState === "not-found" && (
            <p className="rail__navstate rail__navstate--muted" data-testid="teams-tree-not-found">
              No teams for this session
            </p>
          )}

          {teamsState === "error" && (
            <p className="rail__navstate" data-testid="teams-tree-error">
              <span>Couldn’t load teams.</span>
              <button type="button" className="rail__retry" onClick={retryTeams}>
                Retry
              </button>
            </p>
          )}

          {teamsState === "empty" && (
            <p className="rail__navstate rail__navstate--muted" data-testid="teams-tree-empty">
              No teams
            </p>
          )}

          {teamsState === "ok" &&
            teams.map((team) => {
              const isOpen = expandedTeams.has(team.uid);
              const org = orgs[team.uid];
              const teamOnPath = team.uid === activeTeamUid;
              const subId = `rail-team-${team.uid}`;
              return (
                <div key={team.uid} className="rail__treenode">
                  <div className="rail__treerow">
                    <Link
                      href={`/agents?team=${encodeURIComponent(team.uid)}`}
                      className="rail__link rail__link--haschild"
                      data-active={teamOnPath || undefined}
                      title={team.namespace}
                    >
                      <span className="rail__label">{team.name}</span>
                    </Link>
                    <button
                      type="button"
                      className="rail__disclosure"
                      aria-expanded={isOpen}
                      aria-controls={subId}
                      aria-label={isOpen ? `Collapse ${team.name}` : `Expand ${team.name}`}
                      data-testid={`team-toggle-${team.name}`}
                      onClick={() => toggleTeam(team.uid)}
                    >
                      <Caret open={isOpen} />
                    </button>
                  </div>

                  {isOpen && (
                    <div
                      className="rail__sub rail__sub--agents"
                      id={subId}
                      role="group"
                      aria-label={`${team.name} agents`}
                    >
                      {(!org || org.state === "loading") && (
                        <p className="rail__navstate" role="status" data-testid={`agents-loading-${team.name}`}>
                          <span className="rail__spinner" aria-hidden="true" />
                          Loading agents…
                        </p>
                      )}
                      {org?.state === "unconfigured" && (
                        <p className="rail__navstate rail__navstate--muted" data-testid={`agents-unconfigured-${team.name}`}>
                          Agents not wired yet
                        </p>
                      )}
                      {org?.state === "not-found" && (
                        <p className="rail__navstate rail__navstate--muted" data-testid={`agents-not-found-${team.name}`}>
                          No agents for this session
                        </p>
                      )}
                      {org?.state === "error" && (
                        <p className="rail__navstate" data-testid={`agents-error-${team.name}`}>
                          <span>Couldn’t load agents.</span>
                          <button
                            type="button"
                            className="rail__retry"
                            onClick={() => fetchOrg(team.uid)}
                          >
                            Retry
                          </button>
                        </p>
                      )}
                      {org?.state === "empty" && (
                        <p className="rail__navstate rail__navstate--muted" data-testid={`agents-empty-${team.name}`}>
                          No agents
                        </p>
                      )}
                      {org?.state === "ok" &&
                        org.agents.map((agent) => (
                          <Link
                            key={agent.id}
                            href={`/agents/${encodeURIComponent(agent.id)}`}
                            className="rail__link rail__link--leaf"
                            data-active={(agent.id === agentId) || undefined}
                            aria-current={agent.id === agentId ? "page" : undefined}
                          >
                            <span className="rail__label">{agent.name}</span>
                          </Link>
                        ))}
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
