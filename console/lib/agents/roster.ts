"use client";

// lib/agents/roster.ts — the two-axis roster read model for the Agents & Team redesign
// (ISI-5362 / S5, epic ISI-5357; mockups ISI-5306 Frame 01).
//
// The redesign unifies three previously-separate nav nodes (Agents / Teams / Skills) into ONE
// master-detail surface with a two-axis left rail:
//   • ORG          — the population: Teams ▸ Agents (an Agent is grouped under its Team namespace)
//   • LIBRARY      — reusable definitions: Roles, Skills (SHARED objects, not nested under a team)
//   • ORG DEFAULTS — the ModelConfig singleton (the synthetic "Default model" root)
//
// This module is a PURE CONSUMER (R6 scope guard, same posture as lib/agents/api.ts): it composes
// the already-shipped fleet LIST read endpoints and never issues a mutating verb. The two-axis
// separation is honest to the backend — Roles/Skills are library objects an Agent *references*, so
// editing a Role's model has a visible blast radius (the gated Frame 02 edit path, ISI-5358/5359).
//
// Field names mirror the Go wire DTOs (internal/apiserver/fleetlist.go): FleetTeamList /
// FleetAgentList / FleetRoleList / FleetSkillList. buildRoster/filterRoster are pure so the tree
// shape + filter are unit-testable without a network.

// ── Wire shapes (verbatim from fleetlist.go json tags) ───────────────────────

export interface TeamWire {
  name: string;
  namespace: string;
  uid: string;
  agentCount: number;
  projectCount: number;
}

export interface AgentWire {
  id: string;
  name: string;
  namespace: string;
  runtime?: string;
  role?: string;
  coordinator?: boolean;
  model?: string;
  skillCount: number;
}

export interface RoleWire {
  name: string;
  namespace: string;
  uid: string;
  prompt?: string;
  defaultSkills: string[];
  runtimeClassHint?: string;
}

export interface SkillWire {
  name: string;
  namespace: string;
  uid: string;
  teamUid?: string;
  teamName?: string;
  sourceType?: string;
  permissions?: string[];
}

// ── Roster model (what the rail + detail render) ─────────────────────────────

export type RosterNodeKind = "team" | "agent" | "role" | "skill" | "orgDefault";

export interface RosterAgent extends AgentWire {
  kind: "agent";
}

export interface RosterTeam extends TeamWire {
  kind: "team";
  agents: RosterAgent[];
}

export interface RosterRole extends RoleWire {
  kind: "role";
}

export interface RosterSkill extends SkillWire {
  kind: "skill";
}

export interface RosterOrgDefault {
  kind: "orgDefault";
  name: string;
}

export interface Roster {
  teams: RosterTeam[];
  roles: RosterRole[];
  skills: RosterSkill[];
  orgDefault: RosterOrgDefault;
}

/**
 * buildRoster assembles the two-axis roster from the four fleet lists. Agents are grouped under
 * their owning Team by NAMESPACE (the "a squad is a namespace" mapping overview.go/org.go rely on);
 * an Agent whose namespace matches no listed Team is dropped from the ORG axis rather than
 * fabricated under a synthetic team (the lists are already scope-filtered server-side, so an
 * orphan here is a projection skew, not a tenancy leak). Teams and library objects preserve the
 * server's fleet order. Pure — no I/O — so the tree shape is unit-testable.
 */
export function buildRoster(
  teams: TeamWire[],
  agents: AgentWire[],
  roles: RoleWire[],
  skills: SkillWire[],
): Roster {
  const byNamespace = new Map<string, RosterAgent[]>();
  for (const a of agents) {
    const bucket = byNamespace.get(a.namespace);
    const node: RosterAgent = { kind: "agent", ...a };
    if (bucket) bucket.push(node);
    else byNamespace.set(a.namespace, [node]);
  }
  return {
    teams: teams.map((t) => ({
      kind: "team" as const,
      ...t,
      agents: byNamespace.get(t.namespace) ?? [],
    })),
    // ISI-5439(1): the fleet role/skill lists can carry the same library object more than once
    // (aggregated per referencing agent/team server-side), which rendered duplicate rail rows. The
    // rail's UI identity for a library object is its NAME — selection + NodeDetail.resolve key by
    // name, so two rows sharing a name are already indistinguishable here — so dedupe by name,
    // keeping the first occurrence (and thus also collapsing exact uid duplicates). Order preserved.
    roles: dedupeByName(roles).map((r) => ({ kind: "role" as const, ...r })),
    skills: dedupeByName(skills).map((s) => ({ kind: "skill" as const, ...s })),
    orgDefault: { kind: "orgDefault", name: "Default model" },
  };
}

/**
 * dedupeByName keeps the first item for each `name`, preserving input order. Library objects (roles,
 * skills) are identified in this surface by name — their rename-proof key within a squad and the key
 * the rail selection + detail resolver both use — so a repeat name is a duplicate row, not a second
 * object. Pure. (ISI-5439(1).)
 */
export function dedupeByName<T extends { name: string }>(items: T[]): T[] {
  const seen = new Set<string>();
  const out: T[] = [];
  for (const item of items) {
    if (seen.has(item.name)) continue;
    seen.add(item.name);
    out.push(item);
  }
  return out;
}

/**
 * filterRoster narrows the roster to nodes whose name (or, for an agent, its role chip) matches the
 * query case-insensitively. A Team is kept when its own name matches OR any of its agents match —
 * so filtering by a role name ("Backend") surfaces the agents carrying that role without collapsing
 * their team. An empty/whitespace query returns the roster unchanged (same reference). Pure.
 */
export function filterRoster(roster: Roster, query: string): Roster {
  const q = query.trim().toLowerCase();
  if (!q) return roster;
  const hit = (s: string | undefined) => (s ?? "").toLowerCase().includes(q);
  return {
    teams: roster.teams
      .map((t) => {
        if (hit(t.name)) return t;
        const agents = t.agents.filter((a) => hit(a.name) || hit(a.role));
        return agents.length ? { ...t, agents } : null;
      })
      .filter((t): t is RosterTeam => t !== null),
    roles: roster.roles.filter((r) => hit(r.name)),
    skills: roster.skills.filter((s) => hit(s.name)),
    orgDefault: roster.orgDefault,
  };
}

// ── Team detail rollups (ISI-5439(3)) ───────────────────────────────────────

/**
 * teamHierarchy splits a team's agents into coordinator root(s) and the members that report up to
 * them — the data the Team detail's hierarchy diagram draws (coordinator → members edges). We use
 * the one reporting signal the fleet agent list carries, `coordinator`, so the tree is honest to
 * the backend without a heavyweight graph model: coordinators are the roots, everyone else is a
 * member edge beneath them. A team with no flagged coordinator yields an empty `coordinators` list
 * (the diagram then renders a flat member row under the team root). Pure.
 */
export interface TeamHierarchy {
  coordinators: RosterAgent[];
  members: RosterAgent[];
}

export function teamHierarchy(team: RosterTeam): TeamHierarchy {
  const coordinators: RosterAgent[] = [];
  const members: RosterAgent[] = [];
  for (const a of team.agents) {
    if (a.coordinator) coordinators.push(a);
    else members.push(a);
  }
  return { coordinators, members };
}

/**
 * teamSkillSummary rolls up the skills loaded on a team: the union of every role's default skills
 * for the roles that live in the team's namespace, attributed back to the role(s) that load each
 * one. Deduped by skill name, sorted for a stable readable list. This is the honest view from the
 * fleet lists — the agent list carries only a skill COUNT, not names, while roles carry their
 * `defaultSkills` by name, so a role is the finest-grained attribution available without a per-agent
 * detail fetch. Pure. (ISI-5439(3b).)
 */
export interface TeamSkillRollup {
  skill: string;
  /** The role(s) in this team that load the skill as a default, sorted; empty if none attributed. */
  roles: string[];
}

export function teamSkillSummary(roster: Roster, team: RosterTeam): TeamSkillRollup[] {
  const bySkill = new Map<string, Set<string>>();
  for (const role of roster.roles) {
    if (role.namespace !== team.namespace) continue;
    for (const skill of role.defaultSkills) {
      const attributed = bySkill.get(skill) ?? new Set<string>();
      attributed.add(role.name);
      bySkill.set(skill, attributed);
    }
  }
  return [...bySkill.entries()]
    .map(([skill, roles]) => ({ skill, roles: [...roles].sort((a, b) => a.localeCompare(b)) }))
    .sort((a, b) => a.skill.localeCompare(b.skill));
}

// ── Selection ────────────────────────────────────────────────────────────────

/**
 * teamUidForNamespace resolves a node's namespace to its owning Team's UID — the `?team=`
 * act-as-team value the authoring DETAIL reads require for a fleet admin (a name is not
 * fleet-unique; the server resolves the selector by Team UID, never by name). Every rail node
 * (agent, role, skill) carries the namespace of the squad it lives in, and the roster's ORG axis
 * already holds every listed Team, so the selected node's own scope is derivable client-side
 * without a picker. Returns undefined for a namespace no listed Team occupies (projection skew —
 * the caller falls back to the URL selector, if any). Pure.
 */
export function teamUidForNamespace(roster: Roster, namespace: string): string | undefined {
  return roster.teams.find((t) => t.namespace === namespace)?.uid;
}

/**
 * The currently-selected rail node, keyed by its stable identity (Agent by UID; Team by UID;
 * library objects by name — their rename-proof key within a squad). `null` = nothing selected yet
 * (the detail pane shows its placeholder). Discriminated by `kind` so NodeDetail renders the right
 * read-only Overview without a second lookup.
 */
export type RosterSelection =
  | { kind: "team"; uid: string }
  | { kind: "agent"; id: string }
  | { kind: "role"; name: string }
  | { kind: "skill"; name: string }
  | { kind: "orgDefault" }
  | null;

/** True when `sel` points at the given node — the rail's active-row test, kept in one place. */
export function isSelected(
  sel: RosterSelection,
  node: RosterTeam | RosterAgent | RosterRole | RosterSkill | RosterOrgDefault,
): boolean {
  if (!sel) return false;
  switch (node.kind) {
    case "team":
      return sel.kind === "team" && sel.uid === node.uid;
    case "agent":
      return sel.kind === "agent" && sel.id === node.id;
    case "role":
      return sel.kind === "role" && sel.name === node.name;
    case "skill":
      return sel.kind === "skill" && sel.name === node.name;
    case "orgDefault":
      return sel.kind === "orgDefault";
  }
}

// ── Client load ──────────────────────────────────────────────────────────────

export interface RosterState {
  roster: Roster | null;
  loading: boolean;
  /** A non-recoverable load failure (any of the four lists failed) — the surface shows a retry. */
  error: string | null;
}

const LIST_PATHS = [
  "/api/squad/teams",
  "/api/squad/agents",
  "/api/squad/roles",
  "/api/squad/skills",
] as const;

async function getJson<T>(path: string, signal: AbortSignal): Promise<T> {
  const res = await fetch(path, {
    headers: { accept: "application/json" },
    cache: "no-store",
    signal,
  });
  if (!res.ok) throw new Error(`${path} → ${res.status}`);
  return (await res.json()) as T;
}

/**
 * loadRoster fetches the four fleet lists in parallel and assembles the roster. `team` scopes an
 * admin's cross-squad read (the ?team= act-as-team selector the squad reads already honour); a
 * tenant omits it. Rejects if ANY list fails — a partial roster would silently hide a whole axis.
 */
export async function loadRoster(signal: AbortSignal, team?: string): Promise<Roster> {
  const qs = team ? `?team=${encodeURIComponent(team)}` : "";
  const [t, a, r, s] = await Promise.all([
    getJson<{ teams?: TeamWire[] }>(`${LIST_PATHS[0]}${qs}`, signal),
    getJson<{ agents?: AgentWire[] }>(`${LIST_PATHS[1]}${qs}`, signal),
    getJson<{ roles?: RoleWire[] }>(`${LIST_PATHS[2]}${qs}`, signal),
    getJson<{ skills?: SkillWire[] }>(`${LIST_PATHS[3]}${qs}`, signal),
  ]);
  return buildRoster(t.teams ?? [], a.agents ?? [], r.roles ?? [], s.skills ?? []);
}
