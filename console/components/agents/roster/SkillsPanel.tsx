"use client";

// components/agents/roster/SkillsPanel.tsx — the skills editors of the Agents & Team redesign
// (ISI-5362 / S5; mockups ISI-5306 Frame 03).
//
// Two panels sharing one provenance-split chip grammar (ISI-5305 §4.1):
//   • AgentSkillsPanel — the effective union role.defaultSkills ∪ agent.skillRefs, split into
//     "From role <X>" chips (violet — shared, edit them on the role) and "Added to this agent"
//     chips (green — removable ×), plus a "+ Add skill" picker over the Skill library. Editing
//     writes ONLY agent.skillRefs (a field-scoped merge PUT, ISI-5359) — role-tier chips are
//     never touched here.
//   • RoleSkillsPanel — the role's defaultSkills with removable × + the same picker. Saving
//     states the used-by count first (a role is shared: adding a default skill reaches every
//     referencing agent on their next assembly).
//
// Chip colours ride the ISI-4822/4823 provenance mapping (role = violet, agent = green) so
// "where did this come from" reads identically to the effective-model line.
//
// Scope guard (R6): skill BINDINGS only — no skill-definition editing (mcpToolRefs/requires
// stay on the compose skill form), no capability grants, no coordination affordances.

import { useState } from "react";

import type { Roster } from "@/lib/agents/roster";
import {
  agentSkillsPut,
  fetchAgentDetail,
  fetchRoleDetail,
  partitionAgentSkills,
  putCompose,
  roleSkillsPut,
  type AgentDetailWire,
  type RoleDetailWire,
  type SaveState,
} from "@/lib/agents/rosterEdit";
import { useRosterDetail } from "./useRosterDetail";

/** Frame 04's save ladder tail (same grammar as ModelPanel's). */
function SaveLadder({ state }: { state: SaveState }) {
  if (state.kind === "saving") return <p className="roster-edit__ladder roster-edit__ladder--saving">Saving…</p>;
  if (state.kind === "saved") return <p className="roster-edit__ladder roster-edit__ladder--saved">Saved</p>;
  if (state.kind === "error") return <p className="roster-edit__ladder roster-edit__ladder--error">{state.message}</p>;
  return null;
}

/** A chip in the provenance split. `onRemove` absent ⇒ role-tier (not removable here). */
function SkillChip({
  name,
  tier,
  onRemove,
  disabled,
}: {
  name: string;
  tier: "role" | "agent";
  onRemove?: () => void;
  disabled?: boolean;
}) {
  return (
    <span
      className={`chip roster-edit__chip roster-edit__chip--${tier}`}
      title={tier === "role" ? "Granted by the role — edit it on the role" : "Added to this agent"}
    >
      {name}
      {onRemove ? (
        <button
          type="button"
          className="roster-edit__chip-x"
          aria-label={`Remove ${name}`}
          onClick={onRemove}
          disabled={disabled}
        >
          ×
        </button>
      ) : null}
    </span>
  );
}

/** The "+ Add skill" picker: a filter box over the Skill library nodes the roster already holds. */
function SkillPicker({
  roster,
  onPick,
  disabled,
}: {
  roster: Roster;
  onPick: (name: string) => void;
  disabled?: boolean;
}) {
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState("");
  const q = query.trim().toLowerCase();
  const hits = roster.skills.filter((s) => !q || s.name.toLowerCase().includes(q)).slice(0, 8);
  if (!open) {
    return (
      <button type="button" className="btn" onClick={() => setOpen(true)} disabled={disabled}>
        + Add skill
      </button>
    );
  }
  return (
    <div className="roster-edit__picker">
      <input
        className="roster-edit__input roster-edit__picker-input"
        type="text"
        value={query}
        placeholder="Filter skills…"
        autoComplete="off"
        autoFocus
        onChange={(e) => setQuery(e.target.value)}
      />
      {hits.length ? (
        <ul className="roster-edit__picker-list">
          {hits.map((s) => (
            <li key={s.name}>
              <button
                type="button"
                className="roster-edit__picker-option"
                onClick={() => {
                  onPick(s.name);
                  setOpen(false);
                  setQuery("");
                }}
                disabled={disabled}
              >
                {s.name}
                {s.sourceType ? <span className="muted"> · {s.sourceType}</span> : null}
              </button>
            </li>
          ))}
        </ul>
      ) : (
        <p className="muted roster-edit__picker-empty">No skills match.</p>
      )}
      <button
        type="button"
        className="btn"
        onClick={() => {
          setOpen(false);
          setQuery("");
        }}
        disabled={disabled}
      >
        Close
      </button>
    </div>
  );
}

// ── Agent → Skills (provenance-split union) ──────────────────────────────────

export function AgentSkillsPanel({
  agentName,
  roster,
  team,
  onSaved,
}: {
  agentName: string;
  roster: Roster;
  team?: string;
  onSaved: () => void;
}) {
  const [nonce, setNonce] = useState(0);
  const state = useRosterDetail<AgentDetailWire>(
    (signal) => fetchAgentDetail(agentName, signal, team),
    nonce,
  );
  const [save, setSave] = useState<SaveState>({ kind: "idle" });

  if (state.kind === "loading") return <p className="muted">Loading agent…</p>;
  if (state.kind === "error") return <p className="roster-edit__ladder roster-edit__ladder--error">{state.message}</p>;
  const detail = state.detail;

  // The role-tier half of the union: read from the roster's library axis (already loaded —
  // no extra fetch; the rail and this panel agree by construction).
  const role = roster.roles.find((r) => r.name === detail.roleRef.name);
  const { fromRole, addedToAgent } = partitionAgentSkills(detail, role?.defaultSkills ?? []);

  async function saveBindings(next: string[]) {
    setSave({ kind: "saving" });
    try {
      await putCompose("agents", detail.name, agentSkillsPut(detail, next));
      setSave({ kind: "saved" });
      setNonce((n) => n + 1); // re-read skillRefs + refresh the rail's count via the parent
      onSaved();
    } catch (e) {
      setSave({ kind: "error", message: e instanceof Error ? e.message : "Save failed." });
    }
  }

  const busy = save.kind === "saving";

  return (
    <div className="roster-edit">
      <div className="card roster-edit__card">
        <h3 className="roster-detail__card-head">
          From role {role ? role.name : detail.roleRef.name}
        </h3>
        {fromRole.length ? (
          <p className="roster-edit__chips">
            {fromRole.map((s) => (
              <SkillChip key={s} name={s} tier="role" />
            ))}
          </p>
        ) : (
          <p className="muted">The role pins no default skills.</p>
        )}
        <p className="muted roster-detail__note">
          Shared definitions — edit them on the role (their blast radius shows there).
        </p>
      </div>
      <div className="card roster-edit__card">
        <h3 className="roster-detail__card-head">Added to this agent</h3>
        {addedToAgent.length ? (
          <p className="roster-edit__chips">
            {addedToAgent.map((s) => (
              <SkillChip key={s} name={s} tier="agent" onRemove={() => void saveBindings(addedToAgent.filter((x) => x !== s))} disabled={busy} />
            ))}
          </p>
        ) : (
          <p className="muted">No agent-added skills yet.</p>
        )}
        <div className="roster-edit__row">
          <SkillPicker roster={roster} onPick={(name) => void saveBindings([...addedToAgent, name])} disabled={busy} />
        </div>
        <SaveLadder state={save} />
      </div>
    </div>
  );
}

// ── Role → Default Skills (shared library edit) ──────────────────────────────

export function RoleSkillsPanel({
  roleName,
  roster,
  team,
  onSaved,
}: {
  roleName: string;
  roster: Roster;
  team?: string;
  onSaved: () => void;
}) {
  const [nonce, setNonce] = useState(0);
  const state = useRosterDetail<RoleDetailWire>(
    (signal) => fetchRoleDetail(roleName, signal, team),
    nonce,
  );
  const [save, setSave] = useState<SaveState>({ kind: "idle" });

  if (state.kind === "loading") return <p className="muted">Loading role…</p>;
  if (state.kind === "error") return <p className="roster-edit__ladder roster-edit__ladder--error">{state.message}</p>;
  const detail = state.detail;

  const current = (detail.defaultSkills ?? []).map((r) => r.name);
  const affected = detail.usedBy?.agents ?? [];

  async function saveBindings(next: string[]) {
    setSave({ kind: "saving" });
    try {
      await putCompose("roles", detail.name, roleSkillsPut(detail, next));
      setSave({ kind: "saved" });
      setNonce((n) => n + 1);
      onSaved();
    } catch (e) {
      setSave({ kind: "error", message: e instanceof Error ? e.message : "Save failed." });
    }
  }

  const busy = save.kind === "saving";

  return (
    <div className="roster-edit">
      <div className="card roster-edit__card">
        <h3 className="roster-detail__card-head">Default skills</h3>
        {current.length ? (
          <p className="roster-edit__chips">
            {current.map((s) => (
              <SkillChip key={s} name={s} tier="role" onRemove={() => void saveBindings(current.filter((x) => x !== s))} disabled={busy} />
            ))}
          </p>
        ) : (
          <p className="muted">No default skills yet.</p>
        )}
        <div className="roster-edit__row">
          <SkillPicker roster={roster} onPick={(name) => void saveBindings([...current, name])} disabled={busy} />
        </div>
        <SaveLadder state={save} />
      </div>
      {affected.length ? (
        <p className="muted roster-detail__note">
          Reaches {affected.length} {affected.length === 1 ? "agent" : "agents"} ({[...affected].slice(0, 6).join(", ")}
          {affected.length > 6 ? ` +${affected.length - 6} more` : ""}) — every referencing agent
          gains or loses this default on its next assembly.
        </p>
      ) : (
        <p className="muted roster-detail__note">No agents reference this role yet — edits are local to the library.</p>
      )}
    </div>
  );
}
