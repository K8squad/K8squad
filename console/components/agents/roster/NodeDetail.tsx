"use client";

// components/agents/roster/NodeDetail.tsx — the master-detail right pane of the Agents & Team
// redesign (ISI-5362 / S5; mockups ISI-5306 Frame 01/02).
//
// Renders the selected rail node read-only: breadcrumb + name + type badge + tab strip
// (Overview · Model · Skills · Advanced) + a read-only Overview per node kind. The headline is the
// Agent Overview's "effective model" line with server-painted provenance — reused verbatim from the
// shipped <EffectiveModelReadout> (ISI-4822/4892) so "where did this model come from" reads
// identically here and in the compose form.
//
// GATE (ISI-5362 AC): inline EDIT-SAVE is blocked on S1 (ISI-5358 role round-trip) + S2
// (ISI-5359 field-scoped merge writes). Until those land on main, a PUT can silently destroy unsent
// fields — so every mutate affordance here (Edit inline, model pencil, + Add skill) is rendered
// DISABLED with an honest "ships after S1+S2" title. This increment is the read-only surface the
// gated Frame 02 edit will later hang off; nothing here writes.

import { useState } from "react";

import { EffectiveModelReadout } from "@/components/compose/EffectiveModelReadout";
import {
  type Roster,
  type RosterAgent,
  type RosterRole,
  type RosterSelection,
  type RosterSkill,
  type RosterTeam,
} from "@/lib/agents/roster";

const GATE_TITLE =
  "Inline editing ships after ISI-5358 (role round-trip) + ISI-5359 (merge writes) land — until then a save could drop unsent fields.";

/** A mutate affordance that is intentionally inert until the edit backend (S1+S2) merges. */
function GatedAction({ label }: { label: string }) {
  return (
    <button type="button" className="btn" disabled aria-disabled="true" title={GATE_TITLE}>
      {label}
      <span className="roster-detail__soon" aria-hidden="true">
        soon
      </span>
    </button>
  );
}

type NodeTab = "overview" | "model" | "skills" | "advanced";
const TABS: ReadonlyArray<{ key: NodeTab; label: string }> = [
  { key: "overview", label: "Overview" },
  { key: "model", label: "Model" },
  { key: "skills", label: "Skills" },
  { key: "advanced", label: "Advanced" },
];

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

function DetailShell({
  crumb,
  name,
  kind,
  actions,
  children,
}: {
  crumb: string[];
  name: string;
  kind: string;
  actions: React.ReactNode;
  children: React.ReactNode;
}) {
  const [tab, setTab] = useState<NodeTab>("overview");
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
      <div className="roster-detail__tabs" role="tablist">
        {TABS.map((t) => (
          <button
            key={t.key}
            type="button"
            role="tab"
            aria-selected={tab === t.key}
            className={`roster-detail__tab${tab === t.key ? " is-active" : ""}`}
            onClick={() => setTab(t.key)}
          >
            {t.label}
          </button>
        ))}
      </div>
      <div className="roster-detail__body">
        {/* The Overview carries the full read in this increment; the rich editable Model/Skills
            panels are the gated Frame 02/03 follow-ups (ISI-5358/5359), so non-overview tabs show
            a read-only summary rather than a half-built editor. */}
        {tab === "overview" ? children : <TabFallback tab={tab} />}
      </div>
    </section>
  );
}

function TabFallback({ tab }: { tab: NodeTab }) {
  const copy: Record<NodeTab, string> = {
    overview: "",
    model: "The editable model-per-role panel (Frame 02) ships after ISI-5358 + ISI-5359. The current effective model is shown on the Overview tab.",
    skills: "The provenance-split skill chips + capability matrix (Frame 03) ship next. The skill count is shown on the Overview tab.",
    advanced: "Advanced identity and scheduling fields are read-only for now.",
  };
  return <p className="muted">{copy[tab]}</p>;
}

// ── Per-kind Overviews ───────────────────────────────────────────────────────

function AgentOverview({ agent }: { agent: RosterAgent }) {
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
        <EffectiveModelReadout agentName={agent.name} />
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
        Model-per-role editing (with blast-radius) ships after ISI-5358 + ISI-5359.
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

function TeamOverview({ team }: { team: RosterTeam }) {
  return (
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
}: {
  roster: Roster;
  selection: NonNullable<RosterSelection>;
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
    case "agent":
      return (
        <DetailShell
          crumb={["Org", r.node.namespace, r.node.name]}
          name={r.node.name}
          kind="agent"
          actions={
            <>
              <GatedAction label="Edit inline" />
              <GatedAction label="Reassign role" />
            </>
          }
        >
          <AgentOverview agent={r.node} />
        </DetailShell>
      );
    case "team":
      return (
        <DetailShell
          crumb={["Org", r.node.name]}
          name={r.node.name}
          kind="team"
          actions={<GatedAction label="Edit inline" />}
        >
          <TeamOverview team={r.node} />
        </DetailShell>
      );
    case "role":
      return (
        <DetailShell
          crumb={["Library", "Roles", r.node.name]}
          name={r.node.name}
          kind="role"
          actions={<GatedAction label="Edit inline" />}
        >
          <RoleOverview role={r.node} />
        </DetailShell>
      );
    case "skill":
      return (
        <DetailShell
          crumb={["Library", "Skills", r.node.name]}
          name={r.node.name}
          kind="skill"
          actions={<GatedAction label="Edit inline" />}
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
