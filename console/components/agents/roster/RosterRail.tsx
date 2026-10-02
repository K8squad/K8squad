"use client";

// components/agents/roster/RosterRail.tsx — the two-axis left rail of the Agents & Team redesign
// (ISI-5362 / S5; mockups ISI-5306 Frame 01).
//
// Three groups, honest to the backend (DESIGN-SPEC §2):
//   • ORG          — Teams ▸ Agents (disclosure tree; an agent row carries its ·Role chip so the
//                    "group by role" facet is legible without leaving the tree — the novel element)
//   • LIBRARY      — Roles, Skills (SHARED library objects, flat lists with count badges)
//   • ORG DEFAULTS — the "Default model" singleton (links the ModelConfig)
//
// Pure presentational + selection: it owns the open/closed disclosure state and emits selection
// up; it reads nothing and writes nothing. A filter box narrows the tree via the pure filterRoster
// (lib/agents/roster). Node-type icons + count badges make the hierarchy legible at a glance.

import { useMemo, useState } from "react";

import {
  filterRoster,
  isSelected,
  type Roster,
  type RosterAgent,
  type RosterSelection,
  type RosterTeam,
} from "@/lib/agents/roster";

// Node-type glyphs — plain unicode so the rail needs no icon dependency and stays theme-invariant.
// Decorative (aria-hidden): the row's text label is the accessible name.
const ICON = {
  team: "▣",
  agent: "◐",
  role: "▤",
  skill: "✦",
  orgDefault: "✱",
} as const;

function CountBadge({ n }: { n: number }) {
  return <span className="roster-rail__count">({n})</span>;
}

/** An agent row: node icon, name, and the ·Role chip (the "group by role" facet in-tree). */
function AgentRow({
  agent,
  selected,
  onSelect,
}: {
  agent: RosterAgent;
  selected: boolean;
  onSelect: () => void;
}) {
  return (
    <button
      type="button"
      className={`roster-rail__row roster-rail__row--agent${selected ? " is-selected" : ""}`}
      aria-current={selected ? "true" : undefined}
      onClick={onSelect}
    >
      <span className="roster-rail__icon" aria-hidden="true">
        {ICON.agent}
      </span>
      <span className="roster-rail__label">{agent.name}</span>
      {agent.role ? (
        <span className="roster-rail__role-chip" title={`Role: ${agent.role}`}>
          ·{agent.role}
        </span>
      ) : null}
    </button>
  );
}

function TeamGroup({
  team,
  sel,
  onSelect,
}: {
  team: RosterTeam;
  sel: RosterSelection;
  onSelect: (s: RosterSelection) => void;
}) {
  const [open, setOpen] = useState(true);
  const teamSelected = isSelected(sel, team);
  return (
    <li className="roster-rail__node">
      <div className="roster-rail__twisty-row">
        <button
          type="button"
          className="roster-rail__twisty"
          aria-expanded={open}
          aria-label={open ? `Collapse ${team.name}` : `Expand ${team.name}`}
          onClick={() => setOpen((o) => !o)}
        >
          {open ? "▾" : "▸"}
        </button>
        <button
          type="button"
          className={`roster-rail__row roster-rail__row--team${teamSelected ? " is-selected" : ""}`}
          aria-current={teamSelected ? "true" : undefined}
          onClick={() => onSelect({ kind: "team", uid: team.uid })}
        >
          <span className="roster-rail__icon" aria-hidden="true">
            {ICON.team}
          </span>
          <span className="roster-rail__label">{team.name}</span>
          <CountBadge n={team.agentCount} />
        </button>
      </div>
      {open ? (
        <ul className="roster-rail__children">
          {team.agents.length ? (
            team.agents.map((a) => (
              <li key={a.id}>
                <AgentRow
                  agent={a}
                  selected={isSelected(sel, a)}
                  onSelect={() => onSelect({ kind: "agent", id: a.id })}
                />
              </li>
            ))
          ) : (
            <li className="roster-rail__empty muted">No agents on this team yet</li>
          )}
        </ul>
      ) : null}
    </li>
  );
}

/** A flat library row (Role or Skill) with a node-type icon. */
function LibraryRow({
  icon,
  label,
  selected,
  onSelect,
}: {
  icon: string;
  label: string;
  selected: boolean;
  onSelect: () => void;
}) {
  return (
    <li>
      <button
        type="button"
        className={`roster-rail__row roster-rail__row--lib${selected ? " is-selected" : ""}`}
        aria-current={selected ? "true" : undefined}
        onClick={onSelect}
      >
        <span className="roster-rail__icon" aria-hidden="true">
          {icon}
        </span>
        <span className="roster-rail__label">{label}</span>
      </button>
    </li>
  );
}

export function RosterRail({
  roster,
  selection,
  onSelect,
}: {
  roster: Roster;
  selection: RosterSelection;
  onSelect: (s: RosterSelection) => void;
}) {
  const [query, setQuery] = useState("");
  const view = useMemo(() => filterRoster(roster, query), [roster, query]);
  const orgDefaultSelected = isSelected(selection, roster.orgDefault);

  return (
    <nav className="roster-rail" aria-label="Agents, teams, roles and skills">
      <div className="roster-rail__filter">
        <input
          type="search"
          className="roster-rail__filter-input"
          placeholder="Filter teams, roles, skills…"
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          aria-label="Filter the roster"
        />
      </div>

      <div className="roster-rail__group">
        <p className="roster-rail__group-head">Org</p>
        <ul className="roster-rail__tree">
          {view.teams.length ? (
            view.teams.map((t) => (
              <TeamGroup key={t.uid} team={t} sel={selection} onSelect={onSelect} />
            ))
          ) : (
            <li className="roster-rail__empty muted">No matching teams</li>
          )}
        </ul>
      </div>

      <div className="roster-rail__group">
        <p className="roster-rail__group-head">
          Library <CountBadge n={view.roles.length + view.skills.length} />
        </p>
        <ul className="roster-rail__tree">
          <li className="roster-rail__subhead muted">
            Roles <CountBadge n={view.roles.length} />
          </li>
          {view.roles.map((r) => (
            <LibraryRow
              key={r.uid}
              icon={ICON.role}
              label={r.name}
              selected={isSelected(selection, r)}
              onSelect={() => onSelect({ kind: "role", name: r.name })}
            />
          ))}
          <li className="roster-rail__subhead muted">
            Skills <CountBadge n={view.skills.length} />
          </li>
          {view.skills.map((s) => (
            <LibraryRow
              key={s.uid}
              icon={ICON.skill}
              label={s.name}
              selected={isSelected(selection, s)}
              onSelect={() => onSelect({ kind: "skill", name: s.name })}
            />
          ))}
        </ul>
      </div>

      <div className="roster-rail__group">
        <p className="roster-rail__group-head">Org defaults</p>
        <ul className="roster-rail__tree">
          <LibraryRow
            icon={ICON.orgDefault}
            label={roster.orgDefault.name}
            selected={orgDefaultSelected}
            onSelect={() => onSelect({ kind: "orgDefault" })}
          />
        </ul>
      </div>
    </nav>
  );
}
