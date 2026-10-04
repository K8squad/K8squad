"use client";

// components/agents/roster/ModelPanel.tsx — the inline model editor of the Agents & Team
// redesign (ISI-5362 / S5; mockups ISI-5306 Frame 02 "the headline requirement").
//
// Two panels, one interaction grammar (view → edit in place → confirmed save, ISI-5305 §2):
//   • RoleModelPanel  — the shared-library Model row: pencil → inline form → a PESSIMISTIC
//     blast-radius confirm ("Changing this model affects N agents", from→to) before the PUT,
//     because editing a Role re-roots every referencing Agent (spec §2.2).
//   • AgentModelPanel — the per-agent tier: the effective-model readout (server-painted
//     provenance, reused verbatim from compose) + [Override] / [Reset to inherited].
//     Optimistic ladder (single-agent blast radius): Saving… → Saved tick / inline error.
//
// Writes go through lib/agents/rosterEdit.putCompose — a FIELD-SCOPED MERGE (ISI-5359) — so
// each commit sends only its kind's admission-required identity plus the edited field. The
// S1+S2 gate that previously kept these affordances disabled is merged; the wire-level
// agent-model blank=inherit alignment that this path needs rides the same branch.
//
// Scope guard (R6): no coordination affordances live here — model identity only.

import { useState } from "react";

import { EffectiveModelReadout } from "@/components/compose/EffectiveModelReadout";
import { CURATED_MODELS } from "@/lib/modelHints";
import {
  agentModelPut,
  fetchAgentDetail,
  fetchRoleDetail,
  putCompose,
  roleModelPut,
  type AgentDetailWire,
  type RoleDetailWire,
  type SaveState,
} from "@/lib/agents/rosterEdit";
import { useRosterDetail } from "./useRosterDetail";

// ── Shared primitives ────────────────────────────────────────────────────────

/** Frame 04's save ladder tail: the Saving…/Saved/Error line under an editor. */
function SaveLadder({ state }: { state: SaveState }) {
  if (state.kind === "saving") return <p className="roster-edit__ladder roster-edit__ladder--saving">Saving…</p>;
  if (state.kind === "saved") return <p className="roster-edit__ladder roster-edit__ladder--saved">Saved</p>;
  if (state.kind === "error") return <p className="roster-edit__ladder roster-edit__ladder--error">{state.message}</p>;
  return null;
}

/** A plain model id input with the curated catalog as datalist suggestions (AC1-style). */
function ModelInput({
  id,
  value,
  onChange,
  placeholder,
  autoFocus,
}: {
  id: string;
  value: string;
  onChange: (v: string) => void;
  placeholder?: string;
  autoFocus?: boolean;
}) {
  return (
    <>
      <input
        id={id}
        className="roster-edit__input"
        type="text"
        list="roster-model-hints"
        value={value}
        placeholder={placeholder}
        autoComplete="off"
        autoFocus={autoFocus}
        onChange={(e) => onChange(e.target.value)}
      />
      <datalist id="roster-model-hints">
        {CURATED_MODELS.map((m) => (
          <option key={m.id} value={m.id} />
        ))}
      </datalist>
    </>
  );
}

/** Frame 02's pessimistic confirm: the impact count, the from→to diff, the affected list. */
function BlastRadiusConfirm({
  count,
  affected,
  from,
  to,
  onApply,
  onCancel,
  busy,
}: {
  count: number;
  affected: string[];
  from: string;
  to: string;
  onApply: () => void;
  onCancel: () => void;
  busy: boolean;
}) {
  const shown = affected.slice(0, 8);
  return (
    <div className="roster-edit__confirm" role="alertdialog" aria-label="Confirm shared-role change">
      <p className="roster-edit__confirm-head">
        Changing this model affects <strong>{count} {count === 1 ? "agent" : "agents"}</strong>
      </p>
      <p className="roster-edit__confirm-diff">
        <span className="roster-edit__from">{from || "inherit org default"}</span>
        <span className="roster-edit__arrow" aria-hidden="true"> → </span>
        <span className="roster-edit__to">{to || "inherit org default"}</span>
      </p>
      {shown.length ? (
        <ul className="roster-edit__confirm-list">
          {shown.map((a) => (
            <li key={a}>{a}</li>
          ))}
          {affected.length > shown.length ? <li>+{affected.length - shown.length} more</li> : null}
        </ul>
      ) : null}
      <div className="roster-edit__confirm-actions">
        <button type="button" className="btn btn--primary" onClick={onApply} disabled={busy}>
          {busy ? "Applying…" : "Apply"}
        </button>
        <button type="button" className="btn" onClick={onCancel} disabled={busy}>
          Cancel
        </button>
      </div>
    </div>
  );
}

// ── Role → Model (Frame 02) ──────────────────────────────────────────────────

type RoleEditState =
  | { kind: "view" }
  | { kind: "editing" }
  | { kind: "confirm" };

export function RoleModelPanel({
  roleName,
  team,
  onSaved,
}: {
  roleName: string;
  team?: string;
  /** Notifies the workspace to refetch the roster (role chips / readouts elsewhere). */
  onSaved: () => void;
}) {
  const [nonce, setNonce] = useState(0);
  const detailState = useRosterDetail<RoleDetailWire>(
    (signal) => fetchRoleDetail(roleName, signal, team),
    nonce,
  );
  const [edit, setEdit] = useState<RoleEditState>({ kind: "view" });
  const [model, setModel] = useState("");
  const [fallback, setFallback] = useState<string | undefined>(undefined);
  const [save, setSave] = useState<SaveState>({ kind: "idle" });

  if (detailState.kind === "loading") {
    return <p className="muted">Loading role…</p>;
  }
  if (detailState.kind === "error") {
    return <p className="roster-edit__ladder roster-edit__ladder--error">{detailState.message}</p>;
  }
  const detail = detailState.detail;
  const affected = detail.usedBy?.agents ?? [];

  function startEdit() {
    setModel(detail.model ?? "");
    setFallback(undefined); // untouched unless the user edits it
    setSave({ kind: "idle" });
    setEdit({ kind: "editing" });
  }

  function cancelEdit() {
    setEdit({ kind: "view" });
    setSave({ kind: "idle" });
  }

  async function commit() {
    setSave({ kind: "saving" });
    try {
      await putCompose("roles", detail.name, roleModelPut(detail, { model, fallbackModel: fallback }), team);
      setSave({ kind: "saved" });
      setEdit({ kind: "view" });
      setNonce((n) => n + 1); // re-read: provenance + resourceVersion + usedBy refresh
      onSaved();
    } catch (e) {
      setSave({ kind: "error", message: e instanceof Error ? e.message : "Save failed." });
      // Back to the form with the values intact — the user fixes or cancels.
      setEdit({ kind: "editing" });
    }
  }

  function requestSave() {
    // Pessimistic gate only when the shared edit actually reaches other agents (spec §2.2);
    // a role nothing references yet saves on the optimistic ladder.
    if (affected.length) setEdit({ kind: "confirm" });
    else void commit();
  }

  return (
    <div className="roster-edit">
      <div className="card roster-edit__card">
        <h3 className="roster-detail__card-head">Role-tier model</h3>
        {edit.kind === "view" ? (
          <>
            <dl className="roster-detail__dl">
              <dt>Model</dt>
              <dd>
                {detail.model ? (
                  <>
                    <code>{detail.model}</code>
                    <span className="roster-edit__prov roster-edit__prov--role">role default</span>
                  </>
                ) : (
                  <>
                    <span className="muted">inherits org default</span>
                    <span className="roster-edit__prov roster-edit__prov--default">org default</span>
                  </>
                )}
              </dd>
              <dt>Fallback model</dt>
              <dd>
                {detail.fallbackModel?.model ? (
                  <code>{detail.fallbackModel.model}</code>
                ) : (
                  <span className="muted">none</span>
                )}
              </dd>
              <dt>Used by</dt>
              <dd>
                {affected.length ? (
                  <span>
                    {affected.length} {affected.length === 1 ? "agent" : "agents"} reference this role
                  </span>
                ) : (
                  <span className="muted">no agents reference this role yet</span>
                )}
              </dd>
            </dl>
            <div className="roster-edit__row">
              <button type="button" className="btn" onClick={startEdit}>
                Edit model
              </button>
            </div>
          </>
        ) : (
          <>
            <div className="roster-edit__fields">
              <label className="roster-edit__label" htmlFor="roster-role-model">
                Model
              </label>
              <ModelInput
                id="roster-role-model"
                value={model}
                onChange={setModel}
                placeholder="e.g. claude-sonnet-4-5 — blank inherits the org default"
                autoFocus
              />
              <label className="roster-edit__label" htmlFor="roster-role-fallback">
                Fallback model <span className="muted">(optional)</span>
              </label>
              <input
                id="roster-role-fallback"
                className="roster-edit__input"
                type="text"
                value={fallback ?? detail.fallbackModel?.model ?? ""}
                placeholder="secondary model on rate-limit recovery"
                autoComplete="off"
                onChange={(e) => setFallback(e.target.value)}
              />
            </div>
            {edit.kind === "confirm" && save.kind !== "error" ? (
              <BlastRadiusConfirm
                count={affected.length}
                affected={affected}
                from={detail.model ?? ""}
                to={model}
                onApply={() => void commit()}
                onCancel={() => setEdit({ kind: "editing" })}
                busy={save.kind === "saving"}
              />
            ) : null}
            <div className="roster-edit__row">
              <button type="button" className="btn btn--primary" onClick={requestSave} disabled={save.kind === "saving"}>
                Save
              </button>
              <button type="button" className="btn" onClick={cancelEdit} disabled={save.kind === "saving"}>
                Cancel
              </button>
            </div>
          </>
        )}
        <SaveLadder state={save} />
      </div>
      <p className="muted roster-detail__note">
        A role is a shared library object — saving a model here re-roots every agent that references
        it, so the confirm shows the blast radius first.
      </p>
    </div>
  );
}

// ── Agent → Model (override / inherit) ───────────────────────────────────────

export function AgentModelPanel({
  agentName,
  team,
  onSaved,
}: {
  agentName: string;
  team?: string;
  onSaved: () => void;
}) {
  const [nonce, setNonce] = useState(0);
  const detailState = useRosterDetail<AgentDetailWire>(
    (signal) => fetchAgentDetail(agentName, signal, team),
    nonce,
  );
  const [editing, setEditing] = useState(false);
  const [model, setModel] = useState("");
  const [save, setSave] = useState<SaveState>({ kind: "idle" });

  if (detailState.kind === "loading") {
    return <p className="muted">Loading agent…</p>;
  }
  if (detailState.kind === "error") {
    return <p className="roster-edit__ladder roster-edit__ladder--error">{detailState.message}</p>;
  }
  const detail = detailState.detail;

  async function commit(next: string) {
    setSave({ kind: "saving" });
    try {
      await putCompose("agents", detail.name, agentModelPut(detail, next), team);
      setSave({ kind: "saved" });
      setEditing(false);
      setNonce((n) => n + 1); // re-read: the override row + the readout below refresh
      onSaved();
    } catch (e) {
      setSave({ kind: "error", message: e instanceof Error ? e.message : "Save failed." });
    }
  }

  return (
    <div className="roster-edit">
      {/* Server-painted provenance (Agent → Role → org-default), reused verbatim from compose.
          `key` forces a remount (→ refetch) after a save flips the winning tier. */}
      <div className="card roster-edit__card">
        <h3 className="roster-detail__card-head">Effective model</h3>
        <EffectiveModelReadout key={nonce} agentName={detail.name} team={team} />
      </div>
      <div className="card roster-edit__card">
        <h3 className="roster-detail__card-head">Agent-tier override</h3>
        {editing ? (
          <>
            <div className="roster-edit__fields">
              <label className="roster-edit__label" htmlFor="roster-agent-model">
                Model
              </label>
              <ModelInput
                id="roster-agent-model"
                value={model}
                onChange={setModel}
                placeholder="e.g. claude-opus-4-8 — blank resets to inherited"
                autoFocus
              />
            </div>
            <div className="roster-edit__row">
              <button
                type="button"
                className="btn btn--primary"
                onClick={() => void commit(model)}
                disabled={save.kind === "saving"}
              >
                Save
              </button>
              <button type="button" className="btn" onClick={() => setEditing(false)} disabled={save.kind === "saving"}>
                Cancel
              </button>
            </div>
          </>
        ) : (
          <>
            <dl className="roster-detail__dl">
              <dt>Override</dt>
              <dd>
                {detail.model ? (
                  <>
                    <code>{detail.model}</code>
                    <span className="roster-edit__prov roster-edit__prov--agent">agent override</span>
                  </>
                ) : (
                  <span className="muted">none — inherits from the role / org default</span>
                )}
              </dd>
            </dl>
            <div className="roster-edit__row">
              <button
                type="button"
                className="btn"
                onClick={() => {
                  setModel(detail.model ?? "");
                  setSave({ kind: "idle" });
                  setEditing(true);
                }}
              >
                {detail.model ? "Change override" : "Override"}
              </button>
              {detail.model ? (
                <button
                  type="button"
                  className="btn"
                  onClick={() => void commit("")}
                  disabled={save.kind === "saving"}
                  title="Clears the agent-tier model so resolution falls back to the role tier"
                >
                  Reset to inherited
                </button>
              ) : null}
            </div>
          </>
        )}
        <SaveLadder state={save} />
      </div>
    </div>
  );
}
