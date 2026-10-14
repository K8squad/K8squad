"use client";

// components/settings/CredentialsVaultCard.tsx — WS-9 S4 Credentials Vault UI
// (ISI-5847, spec isi-5817). A new "Credentials" section in the project Settings
// tab, sibling to Repository. It renders the per-project credential-types
// projection (GET /api/projects/{id}/credential-types) as a table where the ROWS
// ARE THE CONSUMABLE TYPES (the backend already computed the intersection of the
// reviewed binding registry and the project's [sec]-gated skills), and lets an
// editor materialize a credential via POST /api/credentials {name,value,type,projectId}.
//
// SLICE A: render + Create + status/empty/guided-setup states.
// SLICE B (this slice, ISI-5860 — design O-2 resolved, backed by ISI-5816):
// Rotate (PUT /api/credentials — its OWN explicit verb, delete-then-recreate under
// the stable name, NOT a 409-retry on create) and Delete (DELETE /api/credentials,
// {type} discriminator; 404 = benign "already gone"). The create 409 stays a pure
// guardrail ("use Rotate"), never an auto-overwrite.
//
// ACCEPTANCE-CRITICAL honesty (spec §5/§8), enforced BY CONSTRUCTION here:
//   • the value field is WRITE-ONLY: rendered only for an unsatisfied, editable
//     row (create) OR the active rotate input, NEVER pre-filled, and the typed
//     value is dropped from state the moment a 201/200 lands (masked to ••••••••
//     thereafter). There is NO reveal affordance anywhere in the DOM — not even a
//     password show-toggle (threat T1). No secret value is ever returned to the
//     client on ANY verb (there is no GET for one; rotate never echoes old OR new).
//   • "is it saved?" is read ONLY from gateState — never inferred from any field.
//   • canEdit is the SAME boolean the settings projection exposes (reused from the
//     parent, same ProjectRoleResolver write-tier as Repository): a row shown
//     editable can never hit a 403 (§5).

import { useCallback, useEffect, useMemo, useState, type FormEvent } from "react";
import { EmptyState } from "@/components/forms/EmptyState";
import {
  MASKED_VALUE,
  classifyVaultCreateStatus,
  classifyVaultDeleteStatus,
  classifyVaultRotateStatus,
  createToolCredential,
  deleteToolCredential,
  fetchCredentialTypes,
  isRequiredMissing,
  isSatisfied,
  rotateToolCredential,
  sortVaultTypes,
  vaultCreateMessage,
  vaultDeleteMessage,
  vaultDeleteSucceeded,
  vaultErrorText,
  vaultRotateMessage,
  vaultStatusBadge,
  type CredentialTypeEntry,
  type CredentialTypesState,
} from "@/lib/credentialsVault";

type RowMsg = { tone: "ok" | "bad"; text: string };

export function CredentialsVaultCard({ projectId, canEdit }: { projectId: string; canEdit: boolean }) {
  const [state, setState] = useState<CredentialTypesState>({ kind: "loading" });

  const load = useCallback(async () => {
    try {
      setState(await fetchCredentialTypes(projectId));
    } catch {
      setState({ kind: "error", status: 0 });
    }
  }, [projectId]);

  useEffect(() => {
    void load();
  }, [load]);

  return (
    <section id="settings-credentials" className="card settings__card" data-testid="settings-credentials-panel">
      <div className="settings__card-head">
        <h2 className="settings__card-title">Credentials</h2>
      </div>
      <p className="muted settings__note">
        Tokens the project&apos;s skills need to run. Each row is a credential type a gated skill
        declares — paste a value to connect it. Values are stored as Kubernetes Secrets and are
        never shown back.
      </p>
      <CredentialsBody state={state} projectId={projectId} canEdit={canEdit} onReload={load} />
    </section>
  );
}

function CredentialsBody({
  state,
  projectId,
  canEdit,
  onReload,
}: {
  state: CredentialTypesState;
  projectId: string;
  canEdit: boolean;
  onReload: () => Promise<void>;
}) {
  if (state.kind === "loading") {
    return (
      <div className="card skeleton" aria-busy="true" data-testid="credentials-loading">
        Loading credentials…
      </div>
    );
  }
  if (state.kind === "unauthenticated") {
    return (
      <EmptyState
        testId="credentials-unauthenticated"
        title="Sign in to view credentials"
        why="Your session isn't authenticated for this project."
      />
    );
  }
  if (state.kind === "not-found") {
    return (
      <EmptyState
        testId="credentials-not-found"
        title="Project not found"
        why="No such project in your scope — or you don't have access to it."
      />
    );
  }
  if (state.kind === "not-wired") {
    return (
      <EmptyState
        testId="credentials-not-wired"
        title="Credentials not available yet"
        why="This deployment hasn't wired the credential-types read model (S1). It will appear here once enabled."
      />
    );
  }
  if (state.kind === "error") {
    return (
      <EmptyState
        testId="credentials-error"
        title="Couldn't load credentials"
        why={`The credential-types read model returned an error${state.status ? ` (HTTP ${state.status})` : ""}. Try again.`}
        ctaLabel="Retry"
        onCta={() => void onReload()}
      />
    );
  }

  // Empty types (spec §6): no gated skill declares a consumable credential — a
  // friendly empty card, NOT an error, and NO add button (a free "pick a type"
  // affordance would let a user create a credential no skill consumes).
  if (state.data.types.length === 0) {
    return (
      <EmptyState
        testId="credentials-empty"
        title="No credentials needed yet"
        why="When you enable a skill that requires a token (e.g. GitHub, Dynatrace), it'll show up here for setup."
      />
    );
  }

  return <CredentialsTable types={state.data.types} projectId={projectId} canEdit={canEdit} onReload={onReload} />;
}

function CredentialsTable({
  types,
  projectId,
  canEdit,
  onReload,
}: {
  types: CredentialTypeEntry[];
  projectId: string;
  canEdit: boolean;
  onReload: () => Promise<void>;
}) {
  // Required-but-missing sorts FIRST (spec §6 — guided setup, not an error).
  const rows = useMemo(() => sortVaultTypes(types), [types]);

  // Write-only value inputs, keyed by type. A value is DROPPED from this map the
  // moment its 201 lands — the typed secret never lingers in state (spec §5).
  const [values, setValues] = useState<Record<string, string>>({});
  const [busyType, setBusyType] = useState<string | null>(null);
  const [rowMsg, setRowMsg] = useState<Record<string, RowMsg>>({});

  // Slice B per-row UI state. `rotatingType` is the one row showing its write-only
  // rotate input; `rotateValues` holds those NEW values (same write-only discipline
  // as create — dropped the moment a 200 lands). `confirmingDeleteType` is the one
  // row showing its inline "confirm delete?" affordance (no native confirm() dialog,
  // which would block the event loop and is easy to mis-dismiss for a destructive op).
  const [rotatingType, setRotatingType] = useState<string | null>(null);
  const [rotateValues, setRotateValues] = useState<Record<string, string>>({});
  const [confirmingDeleteType, setConfirmingDeleteType] = useState<string | null>(null);

  const setValue = useCallback((type: string, v: string) => {
    setValues((prev) => ({ ...prev, [type]: v }));
  }, []);

  const setRotateValue = useCallback((type: string, v: string) => {
    setRotateValues((prev) => ({ ...prev, [type]: v }));
  }, []);

  const clearMsg = useCallback((type: string) => {
    setRowMsg((prev) => {
      if (!prev[type]) return prev;
      const next = { ...prev };
      delete next[type];
      return next;
    });
  }, []);

  // Open the rotate input for a row (closing any delete confirm + stale message).
  const startRotate = useCallback(
    (type: string) => {
      setConfirmingDeleteType(null);
      clearMsg(type);
      setRotatingType(type);
    },
    [clearMsg],
  );

  // Cancel rotate — drop the half-typed NEW value so it never lingers (spec §5).
  const cancelRotate = useCallback((type: string) => {
    setRotatingType((cur) => (cur === type ? null : cur));
    setRotateValues((prev) => {
      if (!(type in prev)) return prev;
      const next = { ...prev };
      delete next[type];
      return next;
    });
  }, []);

  const startDelete = useCallback(
    (type: string) => {
      setRotatingType((cur) => (cur === type ? null : cur));
      clearMsg(type);
      setConfirmingDeleteType(type);
    },
    [clearMsg],
  );

  const cancelDelete = useCallback((type: string) => {
    setConfirmingDeleteType((cur) => (cur === type ? null : cur));
  }, []);

  const onSave = useCallback(
    async (e: FormEvent, entry: CredentialTypeEntry) => {
      e.preventDefault();
      const type = entry.type;
      const value = values[type] ?? "";
      setRowMsg((prev) => ({ ...prev, [type]: { tone: "bad", text: "" } }));
      if (!value) {
        setRowMsg((prev) => ({ ...prev, [type]: { tone: "bad", text: "Paste a value for this credential." } }));
        return;
      }
      setBusyType(type);
      try {
        // name is carried for wire-completeness (spec §1.2); the apiserver derives
        // the Secret name from (project, type) and ignores it on the vault axis.
        const res = await createToolCredential({ name: type, value, type, projectId });
        if (res.ok) {
          // Write-only discipline: drop the typed value from state on success so it
          // never lingers — the row re-renders masked off the reloaded gateState.
          setValues((prev) => {
            const next = { ...prev };
            delete next[type];
            return next;
          });
          setRowMsg((prev) => ({ ...prev, [type]: { tone: "ok", text: "Saved." } }));
          await onReload();
        } else {
          const outcome = classifyVaultCreateStatus(res.status);
          const verbatim = await vaultErrorText(res);
          setRowMsg((prev) => ({
            ...prev,
            [type]: { tone: "bad", text: verbatim ?? vaultCreateMessage(outcome) },
          }));
        }
      } catch {
        setRowMsg((prev) => ({ ...prev, [type]: { tone: "bad", text: "The credential store is unreachable — try again." } }));
      } finally {
        setBusyType(null);
      }
    },
    [values, projectId, onReload],
  );

  // Rotate (PUT) — delete-then-recreate under the stable name (design O-2). The NEW
  // value is write-only: dropped from state the instant the 200 lands, so it never
  // lingers (spec §5). A 409 is the rare recreate-after-delete race → "retry", NOT
  // a conflict with an existing value (that guardrail lives on create).
  const onRotate = useCallback(
    async (e: FormEvent, entry: CredentialTypeEntry) => {
      e.preventDefault();
      const type = entry.type;
      const value = rotateValues[type] ?? "";
      setRowMsg((prev) => ({ ...prev, [type]: { tone: "bad", text: "" } }));
      if (!value) {
        setRowMsg((prev) => ({ ...prev, [type]: { tone: "bad", text: "Paste a new value to rotate this credential." } }));
        return;
      }
      setBusyType(type);
      try {
        const res = await rotateToolCredential({ value, type, projectId });
        if (res.ok) {
          // Write-only: drop the NEW value on success; close the rotate input. The
          // row re-renders masked off the reloaded gateState — the value never shows.
          setRotateValues((prev) => {
            const next = { ...prev };
            delete next[type];
            return next;
          });
          setRotatingType(null);
          setRowMsg((prev) => ({ ...prev, [type]: { tone: "ok", text: "Rotated." } }));
          await onReload();
        } else {
          const outcome = classifyVaultRotateStatus(res.status);
          const verbatim = await vaultErrorText(res);
          setRowMsg((prev) => ({
            ...prev,
            [type]: { tone: "bad", text: verbatim ?? vaultRotateMessage(outcome) },
          }));
        }
      } catch {
        setRowMsg((prev) => ({ ...prev, [type]: { tone: "bad", text: "The credential store is unreachable — try again." } }));
      } finally {
        setBusyType(null);
      }
    },
    [rotateValues, projectId, onReload],
  );

  // Delete (DELETE) — blind delete-by-{type}. A 404 is existence-hiding: the honest
  // end state ("no longer stored") is identical to a 200, so both refresh the table
  // rather than surfacing an error (design O-2).
  const onDelete = useCallback(
    async (entry: CredentialTypeEntry) => {
      const type = entry.type;
      setRowMsg((prev) => ({ ...prev, [type]: { tone: "bad", text: "" } }));
      setBusyType(type);
      try {
        const res = await deleteToolCredential({ type, projectId });
        const outcome = classifyVaultDeleteStatus(res.status);
        if (vaultDeleteSucceeded(outcome)) {
          setConfirmingDeleteType(null);
          setRowMsg((prev) => ({ ...prev, [type]: { tone: "ok", text: vaultDeleteMessage(outcome) } }));
          await onReload();
        } else {
          const verbatim = await vaultErrorText(res);
          setRowMsg((prev) => ({
            ...prev,
            [type]: { tone: "bad", text: verbatim ?? vaultDeleteMessage(outcome) },
          }));
        }
      } catch {
        setRowMsg((prev) => ({ ...prev, [type]: { tone: "bad", text: "The credential store is unreachable — try again." } }));
      } finally {
        setBusyType(null);
      }
    },
    [projectId, onReload],
  );

  return (
    <div className="settings__cred-table-wrap">
      <table className="settings__cred-table" data-testid="credentials-vault-table">
        <thead>
          <tr>
            <th scope="col">Name</th>
            <th scope="col">Type</th>
            <th scope="col">Value</th>
            <th scope="col">Consumed by</th>
            <th scope="col">Status</th>
            <th scope="col">Actions</th>
          </tr>
        </thead>
        <tbody>
          {rows.map((entry) => (
            <CredentialRow
              key={entry.type}
              entry={entry}
              canEdit={canEdit}
              value={values[entry.type] ?? ""}
              busy={busyType === entry.type}
              msg={rowMsg[entry.type] ?? null}
              rotating={rotatingType === entry.type}
              rotateValue={rotateValues[entry.type] ?? ""}
              confirmingDelete={confirmingDeleteType === entry.type}
              onChangeValue={(v) => setValue(entry.type, v)}
              onSave={(e) => onSave(e, entry)}
              onChangeRotateValue={(v) => setRotateValue(entry.type, v)}
              onStartRotate={() => startRotate(entry.type)}
              onCancelRotate={() => cancelRotate(entry.type)}
              onRotate={(e) => onRotate(e, entry)}
              onStartDelete={() => startDelete(entry.type)}
              onCancelDelete={() => cancelDelete(entry.type)}
              onConfirmDelete={() => onDelete(entry)}
            />
          ))}
        </tbody>
      </table>
    </div>
  );
}

function CredentialRow({
  entry,
  canEdit,
  value,
  busy,
  msg,
  rotating,
  rotateValue,
  confirmingDelete,
  onChangeValue,
  onSave,
  onChangeRotateValue,
  onStartRotate,
  onCancelRotate,
  onRotate,
  onStartDelete,
  onCancelDelete,
  onConfirmDelete,
}: {
  entry: CredentialTypeEntry;
  canEdit: boolean;
  value: string;
  busy: boolean;
  msg: RowMsg | null;
  rotating: boolean;
  rotateValue: string;
  confirmingDelete: boolean;
  onChangeValue: (v: string) => void;
  onSave: (e: FormEvent) => void;
  onChangeRotateValue: (v: string) => void;
  onStartRotate: () => void;
  onCancelRotate: () => void;
  onRotate: (e: FormEvent) => void;
  onStartDelete: () => void;
  onCancelDelete: () => void;
  onConfirmDelete: () => void;
}) {
  const badge = vaultStatusBadge(entry);
  const satisfied = isSatisfied(entry);
  const requiredMissing = isRequiredMissing(entry);
  const probeless = entry.probeKind === "none";

  return (
    <tr
      className={`settings__cred-row${requiredMissing ? " settings__cred-row--required" : ""}`}
      data-testid={`cred-row-${entry.type}`}
      data-required-missing={requiredMissing ? "true" : undefined}
    >
      {/* Name — the type's display label. Read-only: the backend derives the
          Secret name deterministically from (project, type), so an editable name
          here would be silently dropped (honesty over a dead affordance). */}
      <td data-testid={`cred-name-${entry.type}`}>
        <span className="settings__cred-name">{entry.displayName}</span>
        {requiredMissing ? (
          <span className="muted settings__cred-hint" data-testid={`cred-required-hint-${entry.type}`}>
            This skill can&apos;t run until you add this credential.
          </span>
        ) : null}
      </td>

      {/* Type — the registry key (read-only; the row IS the type). */}
      <td>
        <code className="settings__cred-type" data-testid={`cred-type-${entry.type}`}>
          {entry.type}
        </code>
      </td>

      {/* Value — write-only. While ROTATING a satisfied row, a fresh write-only
          input for the NEW value (same discipline: never pre-filled, NO reveal);
          otherwise masked once satisfied, a write-only create input for an editable
          unsatisfied row, or an honest dash. NEVER pre-filled; NO reveal anywhere. */}
      <td>
        {satisfied && rotating && canEdit ? (
          <input
            type="password"
            className="settings__cred-input"
            value={rotateValue}
            onChange={(e) => onChangeRotateValue(e.target.value)}
            placeholder="Paste the new token"
            data-testid={`cred-rotate-value-${entry.type}`}
            autoComplete="off"
            form={`cred-rotate-form-${entry.type}`}
            aria-label={`New value for ${entry.displayName}`}
            autoFocus
          />
        ) : satisfied ? (
          <span className="settings__cred-masked" data-testid={`cred-masked-${entry.type}`} aria-label="value saved (hidden)">
            {MASKED_VALUE}
          </span>
        ) : canEdit ? (
          <input
            type="password"
            className="settings__cred-input"
            value={value}
            onChange={(e) => onChangeValue(e.target.value)}
            placeholder="Paste the token"
            data-testid={`cred-value-${entry.type}`}
            autoComplete="off"
            form={`cred-form-${entry.type}`}
          />
        ) : (
          <span className="muted" data-testid={`cred-value-readonly-${entry.type}`}>
            —
          </span>
        )}
      </td>

      {/* Consumed by — the skills/MCPServers that read this type. */}
      <td>
        <span className="settings__cred-chips" data-testid={`cred-consumers-${entry.type}`}>
          {entry.consumedBy.length ? (
            entry.consumedBy.map((c) => (
              <span key={c} className="settings__chip settings__chip--muted">
                {c}
              </span>
            ))
          ) : (
            <span className="muted">—</span>
          )}
        </span>
      </td>

      {/* Status — derived badge (spec §3). */}
      <td>
        <span
          className={`settings__badge settings__badge--${badge.tone}`}
          data-testid={`cred-status-${entry.type}`}
          data-tone={badge.tone}
        >
          {badge.label}
        </span>
      </td>

      {/* Actions — Save (create), Test, Rotate/Delete (Slice B). */}
      <td>
        <div className="settings__cred-actions">
          {canEdit && !satisfied ? (
            <form id={`cred-form-${entry.type}`} onSubmit={onSave} className="settings__cred-save-form">
              <button className="btn btn--primary" type="submit" disabled={busy} data-testid={`cred-save-${entry.type}`}>
                {busy ? "Saving…" : "Save"}
              </button>
            </form>
          ) : null}

          {/* Test — Slice A renders it, disabled: probeless types have no probe
              (spec §7), and a tool-credential probe seam addressed by (project,
              type) is not wired yet (needs S3/O-2). Honest tooltip, never a
              fabricated green. */}
          <button
            className="btn"
            type="button"
            disabled
            data-testid={`cred-test-${entry.type}`}
            title={
              probeless
                ? "No connectivity test for this type yet."
                : "Connectivity test ships once the credential probe is wired."
            }
          >
            Test
          </button>

          {/* Rotate / Delete — Slice B (design O-2, backed by ISI-5816). Only for an
              editor on a SAVED credential. Three mutually-exclusive modes:
                • idle           → Rotate + Delete buttons
                • rotating       → Confirm rotate (submits the PUT form whose input
                                   lives in the Value cell) + Cancel
                • confirmingDelete → Confirm delete (danger) + Cancel — an inline
                                   two-step guard, not a blocking native confirm(). */}
          {canEdit && satisfied && rotating ? (
            <form id={`cred-rotate-form-${entry.type}`} onSubmit={onRotate} className="settings__cred-save-form">
              <button
                className="btn btn--primary"
                type="submit"
                disabled={busy}
                data-testid={`cred-rotate-confirm-${entry.type}`}
              >
                {busy ? "Rotating…" : "Confirm rotate"}
              </button>
              <button
                className="btn"
                type="button"
                onClick={onCancelRotate}
                disabled={busy}
                data-testid={`cred-rotate-cancel-${entry.type}`}
              >
                Cancel
              </button>
            </form>
          ) : canEdit && satisfied && confirmingDelete ? (
            <span className="settings__cred-confirm" data-testid={`cred-delete-confirm-wrap-${entry.type}`}>
              <span className="muted settings__cred-hint">Delete this credential?</span>
              <button
                className="btn btn--danger"
                type="button"
                onClick={onConfirmDelete}
                disabled={busy}
                data-testid={`cred-delete-confirm-${entry.type}`}
              >
                {busy ? "Deleting…" : "Confirm delete"}
              </button>
              <button
                className="btn"
                type="button"
                onClick={onCancelDelete}
                disabled={busy}
                data-testid={`cred-delete-cancel-${entry.type}`}
              >
                Cancel
              </button>
            </span>
          ) : canEdit && satisfied ? (
            <>
              <button
                className="btn"
                type="button"
                onClick={onStartRotate}
                disabled={busy}
                data-testid={`cred-rotate-${entry.type}`}
                title="Replace this credential with a new value."
              >
                Rotate
              </button>
              <button
                className="btn btn--danger"
                type="button"
                onClick={onStartDelete}
                disabled={busy}
                data-testid={`cred-delete-${entry.type}`}
                title="Remove this stored credential."
              >
                Delete
              </button>
            </>
          ) : null}
        </div>
        {msg && msg.text ? (
          <p className={`settings__msg settings__msg--${msg.tone}`} data-testid={`cred-msg-${entry.type}`} role="status">
            {msg.text}
          </p>
        ) : null}
      </td>
    </tr>
  );
}
