"use client";

// components/agents/roster/NodeDetail.tsx — the master-detail right pane of the Agents & Team
// redesign (ISI-5362 / S5; mockups ISI-5306 Frame 01/02/03).
//
// Renders the selected rail node: breadcrumb + name + type badge + tab strip
// (Overview · Model · Skills · Advanced) + per-tab content. The Overview is the read-only
// composite (role chip, server-painted effective-model provenance, skill summary); the Model
// and Skills tabs host the INLINE EDITORS (Frame 02/03) — RoleModelPanel / AgentModelPanel /
// AgentSkillsPanel / RoleSkillsPanel — which write through the field-scoped merge PUT
// (ISI-5359) now that the S1 (ISI-5358) + S2 gates are merged on main. The editors are keyed
// by node identity so switching nodes remounts them with fresh detail reads.
//
// Full-form editing (identity, runtime, credentials) stays on the compose screen — the header
// action deep-links there rather than duplicating the wizard. It also carries NO coordination
// affordance (claim/assign/dispatch) — that stays server-side (R6 scope guard, see
// test/agents/no-coordination.test.ts).

import { useState, type ReactNode } from "react";

import { EffectiveModelReadout } from "@/components/compose/EffectiveModelReadout";
import {
  type Roster,
  type RosterAgent,
  type RosterRole,
  type RosterSelection,
  type RosterSkill,
  type RosterTeam,
  teamHierarchy,
  teamSkillSummary,
  teamUidForNamespace,
} from "@/lib/agents/roster";
import { AgentModelPanel, RoleModelPanel } from "./ModelPanel";
import { AgentSkillsPanel, RoleSkillsPanel } from "./SkillsPanel";

type NodeTab = "overview" | "model" | "skills" | "advanced";
const TAB_LABELS: Record<NodeTab, string> = {
  overview: "Overview",
  model: "Model",
  skills: "Skills",
  advanced: "Advanced",
};

// ISI-5439(2,3c): the tab strip is context-aware per node kind, not a fixed four-tab set reused
// everywhere. The rule: a tab appears only when it has meaning for that node.
//   • Model   — only an Agent or Role carries an effective-model override to read/edit here. A
//               Team, a Skill and the Org-default have no per-node model in this pane.
//   • Skills  — the inline skill-grant editor exists only for an Agent (granted skills) and a Role
//               (default skills). A Team's skills are a read-only rollup shown on Overview; a Skill
//               has no sub-skills.
//   • Advanced — identity/scheduling fields exist for an Agent or Role; nothing for the others.
// So Agents/Roles keep the full strip; Teams, Skills and the Org-default collapse to Overview only
// (the Team's diagram + skills rollup live on its Overview). A single-tab node hides the strip.
const TABS_FOR_KIND: Record<string, ReadonlyArray<NodeTab>> = {
  agent: ["overview", "model", "skills", "advanced"],
  role: ["overview", "model", "skills", "advanced"],
  team: ["overview"],
  skill: ["overview"],
  orgDefault: ["overview"],
};

function TypeBadge({ kind }: { kind: string }) {
  return <span className={`roster-detail__badge roster-detail__badge--${kind}`}>{kind}</span>;
}

function Breadcrumb({ parts }: { parts: string[] }) {
  return (
    <p className="roster-detail__crumb muted">
      {parts.map((p, i) => (
        <span key={i}>
          {i > 0 ? <span className="roster-detail__crumb-sep"> / </span> : null}
          {p}
        </span>
      ))}
    </p>
  );
}

/** Deep-link into the compose wizard's edit mode — the full-form editor for this node. */
function EditInCompose({ kind, name }: { kind: string; name: string }) {
  const href = `/compose?kind=${encodeURIComponent(kind)}&mode=edit&name=${encodeURIComponent(name)}`;
  return (
    <a className="btn roster-detail__compose-link" href={href}>
      Full-form edit
    </a>
  );
}

/**
 * ISI-5432: the agent drill-in to the (kept) /agents/{id} detail route — status, live run
 * history and logs live there, not in the roster pane. With the legacy /agents list retired
 * this link is the surface's discoverable path to it.
 */
function AgentRunsLink({ agentId }: { agentId: string }) {
  return (
    <a
      className="btn roster-detail__compose-link"
      href={`/agents/${encodeURIComponent(agentId)}`}
    >
      Runs &amp; detail
    </a>
  );
}

function DetailShell({
  crumb,
  name,
  kind,
  actions,
  tabContent = {},
  children,
}: {
  crumb: string[];
  name: string;
  kind: string;
  actions: ReactNode;
  /** Per-tab bodies; a missing entry falls back to the tab's read-only copy. */
  tabContent?: Partial<Record<NodeTab, ReactNode>>;
  children: ReactNode;
}) {
  // The visible tabs are derived from the node kind (ISI-5439): an irrelevant tab is never rendered
  // nor selectable, so there is no Model tab on a Skill or Team. Agents/Roles get the full strip.
  const tabs = TABS_FOR_KIND[kind] ?? ["overview"];
  const [tab, setTab] = useState<NodeTab>("overview");
  // Guard against a stale selection if the active tab isn't offered for this kind (kind changes as
  // the shell is reused across selections): fall back to Overview, which every kind always has.
  const active = tabs.includes(tab) ? tab : "overview";
  return (
    <section className="roster-detail">
      <header className="roster-detail__head">
        <div className="roster-detail__head-main">
          <Breadcrumb parts={crumb} />
          <h2 className="roster-detail__name">
            {name} <TypeBadge kind={kind} />
          </h2>
        </div>
        <div className="roster-detail__actions">{actions}</div>
      </header>
      {tabs.length > 1 ? (
        <div className="roster-detail__tabs" role="tablist">
          {tabs.map((key) => (
            <button
              key={key}
              type="button"
              role="tab"
              aria-selected={active === key}
              className={`roster-detail__tab${active === key ? " is-active" : ""}`}
              onClick={() => setTab(key)}
            >
              {TAB_LABELS[key]}
            </button>
          ))}
        </div>
      ) : null}
      <div className="roster-detail__body">
        {active === "overview" ? children : (tabContent[active] ?? <TabFallback tab={active} />)}
      </div>
    </section>
  );
}

function TabFallback({ tab }: { tab: NodeTab }) {
  const copy: Record<NodeTab, string> = {
    overview: "",
    model: "The effective model is shown on the Overview tab.",
    skills: "The skill summary is shown on the Overview tab.",
    advanced: "Advanced identity and scheduling fields are read-only here — use Full-form edit.",
  };
  return <p className="muted">{copy[tab]}</p>;
}

// ── Per-kind Overviews ───────────────────────────────────────────────────────

function AgentOverview({ agent, team }: { agent: RosterAgent; team?: string }) {
  return (
    <div className="roster-detail__grid">
      <div className="card roster-detail__card">
        <h3 className="roster-detail__card-head">Role &amp; membership</h3>
        <dl className="roster-detail__dl">
          <dt>Role</dt>
          <dd>
            {agent.role ? (
              <span className="roster-detail__role">{agent.role}</span>
            ) : (
              <span className="muted">No role assigned</span>
            )}
            {agent.coordinator ? (
              <span className="roster-detail__coord" title="Team coordinator role">
                coordinator
              </span>
            ) : null}
          </dd>
          <dt>Runtime</dt>
          <dd>{agent.runtime ?? <span className="muted">—</span>}</dd>
          <dt>Skills</dt>
          <dd>{agent.skillCount} granted</dd>
        </dl>
      </div>
      <div className="card roster-detail__card">
        <h3 className="roster-detail__card-head">Effective model</h3>
        {/* Reused shipped read-out: server-side resolver paints Agent → Role → Org-default
            provenance. Keyed by the persisted agent name. */}
        <EffectiveModelReadout agentName={agent.name} team={team} />
      </div>
    </div>
  );
}

function RoleOverview({ role }: { role: RosterRole }) {
  return (
    <div className="card roster-detail__card">
      <h3 className="roster-detail__card-head">Role definition</h3>
      <dl className="roster-detail__dl">
        <dt>Prompt</dt>
        <dd>{role.prompt ? role.prompt : <span className="muted">— none referenced</span>}</dd>
        <dt>Default skills</dt>
        <dd>
          {role.defaultSkills.length ? (
            <span className="roster-detail__chips">
              {role.defaultSkills.map((s) => (
                <span key={s} className="chip">
                  {s}
                </span>
              ))}
            </span>
          ) : (
            <span className="muted">none</span>
          )}
        </dd>
        <dt>Runtime class</dt>
        <dd>{role.runtimeClassHint ?? <span className="muted">—</span>}</dd>
      </dl>
      <p className="muted roster-detail__note">
        A shared library object — model and default-skill edits on the Model / Skills tabs show
        their blast radius before saving.
      </p>
    </div>
  );
}

function SkillOverview({ skill }: { skill: RosterSkill }) {
  return (
    <div className="card roster-detail__card">
      <h3 className="roster-detail__card-head">Skill</h3>
      <dl className="roster-detail__dl">
        <dt>Source</dt>
        <dd>{skill.sourceType ?? <span className="muted">—</span>}</dd>
        <dt>Owning team</dt>
        <dd>{skill.teamName ?? <span className="muted">—</span>}</dd>
        <dt>Permissions</dt>
        <dd>
          {skill.permissions && skill.permissions.length ? (
            <span className="roster-detail__chips">
              {skill.permissions.map((p) => (
                <span key={p} className="chip">
                  {p}
                </span>
              ))}
            </span>
          ) : (
            <span className="muted">none declared</span>
          )}
        </dd>
      </dl>
    </div>
  );
}

/**
 * TeamHierarchyDiagram — a lightweight node/edge tree of the team's reporting structure
 * (ISI-5439(3a)): coordinator root(s) at the top, member agents as edges beneath. No graph library —
 * a nested list painted with CSS connectors keeps it theme-invariant and accessible (it reads as a
 * real list to a screen reader). When a team has no flagged coordinator the members hang directly
 * off the team root so the structure is still legible.
 */
function TeamHierarchyDiagram({ team }: { team: RosterTeam }) {
  const { coordinators, members } = teamHierarchy(team);
  if (!team.agents.length) {
    return <p className="muted">No agents on this team yet.</p>;
  }
  const DiagramNode = ({ label, role, coord }: { label: string; role?: string; coord?: boolean }) => (
    <span className={`roster-diagram__node${coord ? " roster-diagram__node--coord" : ""}`}>
      <span className="roster-diagram__node-name">{label}</span>
      {role ? <span className="roster-diagram__node-role">·{role}</span> : null}
      {coord ? <span className="roster-diagram__node-tag">coordinator</span> : null}
    </span>
  );
  // A static diagram, not an interactive tree widget — a labelled group wrapping a semantic nested
  // list reads correctly to a screen reader without implying selectable tree items.
  return (
    <div className="roster-diagram" role="group" aria-label={`${team.name} reporting structure`}>
      <div className="roster-diagram__root">
        <DiagramNode label={team.name} />
      </div>
      {coordinators.length ? (
        <ul className="roster-diagram__level">
          {coordinators.map((c) => (
            <li key={c.id} className="roster-diagram__branch">
              <DiagramNode label={c.name} role={c.role} coord />
              {members.length ? (
                <ul className="roster-diagram__level">
                  {members.map((m) => (
                    <li key={m.id}>
                      <DiagramNode label={m.name} role={m.role} />
                    </li>
                  ))}
                </ul>
              ) : null}
            </li>
          ))}
        </ul>
      ) : (
        // No coordinator flagged — members hang off the team root directly (flat structure).
        <ul className="roster-diagram__level">
          {members.map((m) => (
            <li key={m.id}>
              <DiagramNode label={m.name} role={m.role} />
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

function TeamSkillsSummary({ roster, team }: { roster: Roster; team: RosterTeam }) {
  const rollup = teamSkillSummary(roster, team);
  if (!rollup.length) {
    return <p className="muted">No default skills are declared by this team&rsquo;s roles.</p>;
  }
  return (
    <ul className="roster-detail__skill-rollup">
      {rollup.map((r) => (
        <li key={r.skill} className="roster-detail__skill-rollup-row">
          <span className="chip">{r.skill}</span>
          {r.roles.length ? (
            <span className="muted roster-detail__skill-rollup-by">
              via {r.roles.join(", ")}
            </span>
          ) : null}
        </li>
      ))}
    </ul>
  );
}

function TeamOverview({ roster, team }: { roster: Roster; team: RosterTeam }) {
  return (
    <div className="roster-detail__grid">
      <div className="card roster-detail__card">
        <h3 className="roster-detail__card-head">Team</h3>
        <dl className="roster-detail__dl">
          <dt>Agents</dt>
          <dd>{team.agentCount}</dd>
          <dt>Projects</dt>
          <dd>{team.projectCount}</dd>
          <dt>Namespace</dt>
          <dd>
            <code>{team.namespace}</code>
          </dd>
        </dl>
      </div>
      <div className="card roster-detail__card">
        <h3 className="roster-detail__card-head">Reporting structure</h3>
        <TeamHierarchyDiagram team={team} />
      </div>
      <div className="card roster-detail__card">
        <h3 className="roster-detail__card-head">Skills loaded on the team</h3>
        <TeamSkillsSummary roster={roster} team={team} />
        <p className="muted roster-detail__note">
          The union of default skills across this team&rsquo;s roles, attributed to the role that
          loads each one.
        </p>
      </div>
    </div>
  );
}

function OrgDefaultOverview() {
  return (
    <div className="card roster-detail__card">
      <h3 className="roster-detail__card-head">Org default model</h3>
      <p className="muted">
        The organisation-wide default model (the <code>ModelConfig</code> singleton) is the final
        tier in the Agent → Role → Org-default resolution. Manage it in Settings → Configuration.
      </p>
    </div>
  );
}

// ── Resolver ─────────────────────────────────────────────────────────────────

/** Look the selected node up in the roster. Returns null when the selection points at a node that
 * is no longer present (e.g. filtered away then the roster refreshed) — the pane shows a placeholder. */
function resolve(
  roster: Roster,
  selection: NonNullable<RosterSelection>,
):
  | { kind: "team"; node: RosterTeam }
  | { kind: "agent"; node: RosterAgent }
  | { kind: "role"; node: RosterRole }
  | { kind: "skill"; node: RosterSkill }
  | { kind: "orgDefault" }
  | null {
  switch (selection.kind) {
    case "team": {
      const node = roster.teams.find((t) => t.uid === selection.uid);
      return node ? { kind: "team", node } : null;
    }
    case "agent": {
      for (const t of roster.teams) {
        const node = t.agents.find((a) => a.id === selection.id);
        if (node) return { kind: "agent", node };
      }
      return null;
    }
    case "role": {
      const node = roster.roles.find((r) => r.name === selection.name);
      return node ? { kind: "role", node } : null;
    }
    case "skill": {
      const node = roster.skills.find((s) => s.name === selection.name);
      return node ? { kind: "skill", node } : null;
    }
    case "orgDefault":
      return { kind: "orgDefault" };
  }
}

export function NodeDetail({
  roster,
  selection,
  team,
  onRosterChanged,
}: {
  roster: Roster;
  selection: NonNullable<RosterSelection>;
  /** The admin cross-squad URL selector — the FALLBACK team scope; the selected node's own
   *  owning-team UID (derived from its namespace) wins so a fleet admin's detail reads always
   *  target the squad of the node they clicked (ISI-5416). */
  team?: string;
  /** An inline editor saved — the workspace refetches the roster (counts, chips, readouts). */
  onRosterChanged: () => void;
}) {
  const r = resolve(roster, selection);
  if (!r) {
    return (
      <section className="roster-detail">
        <p className="muted">That item is no longer in the roster.</p>
      </section>
    );
  }

  switch (r.kind) {
    case "agent": {
      // ISI-5416: the detail reads need the OWNING team's UID — the rail is fleet-wide for an
      // admin, so the URL selector can be absent (or point elsewhere) while the clicked node
      // still carries its own squad namespace. The node-derived scope wins; URL is the fallback.
      const nodeTeam = teamUidForNamespace(roster, r.node.namespace) ?? team;
      return (
        <DetailShell
          crumb={["Org", r.node.namespace, r.node.name]}
          name={r.node.name}
          kind="agent"
          actions={
            <>
              <AgentRunsLink agentId={r.node.id} />
              <EditInCompose kind="agents" name={r.node.name} />
            </>
          }
          tabContent={{
            // Keyed by node identity: switching agents remounts the editors with fresh reads.
            model: (
              <AgentModelPanel key={r.node.name} agentName={r.node.name} team={nodeTeam} onSaved={onRosterChanged} />
            ),
            skills: (
              <AgentSkillsPanel
                key={r.node.name}
                agentName={r.node.name}
                roster={roster}
                team={nodeTeam}
                onSaved={onRosterChanged}
              />
            ),
          }}
        >
          <AgentOverview agent={r.node} team={nodeTeam} />
        </DetailShell>
      );
    }
    case "team":
      return (
        <DetailShell
          crumb={["Org", r.node.name]}
          name={r.node.name}
          kind="team"
          actions={<EditInCompose kind="teams" name={r.node.name} />}
        >
          <TeamOverview roster={roster} team={r.node} />
        </DetailShell>
      );
    case "role": {
      // Same node-derived act-as-team as the agent case — a Role lives in a squad namespace the
      // roster's ORG axis already knows, so the detail read scopes to the clicked node's team.
      const nodeTeam = teamUidForNamespace(roster, r.node.namespace) ?? team;
      return (
        <DetailShell
          crumb={["Library", "Roles", r.node.name]}
          name={r.node.name}
          kind="role"
          actions={<EditInCompose kind="roles" name={r.node.name} />}
          tabContent={{
            model: (
              <RoleModelPanel key={r.node.name} roleName={r.node.name} team={nodeTeam} onSaved={onRosterChanged} />
            ),
            skills: (
              <RoleSkillsPanel
                key={r.node.name}
                roleName={r.node.name}
                roster={roster}
                team={nodeTeam}
                onSaved={onRosterChanged}
              />
            ),
          }}
        >
          <RoleOverview role={r.node} />
        </DetailShell>
      );
    }
    case "skill":
      return (
        <DetailShell
          crumb={["Library", "Skills", r.node.name]}
          name={r.node.name}
          kind="skill"
          actions={<EditInCompose kind="skills" name={r.node.name} />}
        >
          <SkillOverview skill={r.node} />
        </DetailShell>
      );
    case "orgDefault":
      return (
        <DetailShell
          crumb={["Org defaults", roster.orgDefault.name]}
          name={roster.orgDefault.name}
          kind="orgDefault"
          actions={null}
        >
          <OrgDefaultOverview />
        </DetailShell>
      );
  }
}
