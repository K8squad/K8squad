// components/github/IssueTriageDialog.tsx — ISI-5595 WS-E: the "Issue auto-triage"
// config dialog on the GitHub Issues panel header.
//
// A near-clone of ReviewAutomationDialog (ISI-4764) for visual + behavioural
// parity: a modal over the issue-triage sub-resource (fetchIssueTriage /
// saveIssueTriage → BFF app/api/projects/[id]/repo/issue-triage → apiserver
// issuetriage.go). It exposes the WS-B config knobs:
//   (a) Enable toggle,
//   (b) Triage agent dropdown (any team agent — NO code_review pre-filter),
//   (c) Label filter (triage only issues carrying one of these labels),
//   (d) Only-unassigned toggle.
//
// HONESTY / authZ discipline (carried from ReviewAutomationDialog): the apiserver
// is the wall. This dialog performs NO client-side authZ — it gates the form on the
// server's `canEdit` (read-only when false) and surfaces the write-time 422 inline.
// Unlike review-automation the agent dropdown lists ALL team agents (listSquadAgents):
// issue triage needs no special capability; the agent-∈-Team rule is the server's
// dispatch-time backstop. `enabledBy` is server-stamped and shown read-only.

"use client";

import { useCallback, useEffect, useId, useRef, useState } from "react";
import {
  fetchIssueTriage,
  saveIssueTriage,
  type IssueTriageInput,
  type IssueTriageState,
} from "@/lib/github-status";
import { agentOptionLabel, listSquadAgents, type AgentOption } from "@/lib/tickets/api";

/** The form's mutable state — the write-input subset, resolved from the view. */
type Form = IssueTriageInput;

/** Parse the comma/newline-separated label textbox into a trimmed, de-duped list
 * (empty entries dropped) — the wire shape the apiserver stores verbatim. */
function parseLabels(raw: string): string[] {
  const seen = new Set<string>();
  const out: string[] = [];
  for (const part of raw.split(/[\n,]/)) {
    const t = part.trim();
    if (t && !seen.has(t)) {
      seen.add(t);
      out.push(t);
    }
  }
  return out;
}

export function IssueTriageDialog({
  projectId,
  onClose,
}: {
  projectId: string;
  onClose: () => void;
}) {
  const [state, setState] = useState<IssueTriageState>({ kind: "loading" });
  const [form, setForm] = useState<Form | null>(null);
  // The raw label textbox value (kept separate so a trailing comma while typing
  // isn't eaten); parsed into form.labelFilter on change.
  const [labelsRaw, setLabelsRaw] = useState("");
  const [agents, setAgents] = useState<AgentOption[]>([]);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<{ message: string; fields: string[] } | null>(null);

  const titleId = useId();
  const enableId = useId();
  const agentFieldId = useId();
  const labelsId = useId();
  const onlyUnassignedId = useId();
  const firstFieldRef = useRef<HTMLInputElement>(null);

  // Load the current config; populate the form from the server's resolved defaults.
  useEffect(() => {
    let alive = true;
    void fetchIssueTriage(projectId).then((next) => {
      if (!alive) return;
      setState(next);
      if (next.kind === "ready") {
        const { enabled, triageAgentId, labelFilter, onlyUnassigned } = next.view;
        setForm({ enabled, triageAgentId, labelFilter, onlyUnassigned });
        setLabelsRaw(labelFilter.join(", "));
      }
    });
    return () => {
      alive = false;
    };
  }, [projectId]);

  // Populate the agent dropdown from the squad roster (all team agents). Best-effort
  // — failure degrades to an empty roster so the field still renders "Select an
  // agent" alone, and the write-time 422 stays the authoritative backstop.
  useEffect(() => {
    let alive = true;
    void listSquadAgents().then((list) => {
      if (alive) setAgents(list);
    });
    return () => {
      alive = false;
    };
  }, []);

  // Close on Escape; focus the first field on open.
  useEffect(() => {
    firstFieldRef.current?.focus();
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [onClose]);

  const view = state.kind === "ready" ? state.view : null;
  const canEdit = view?.canEdit === true;

  const patch = useCallback((p: Partial<Form>) => {
    setForm((f) => (f ? { ...f, ...p } : f));
    setError(null);
  }, []);

  const submit = useCallback(async () => {
    if (!form || !canEdit || saving) return;
    setSaving(true);
    setError(null);
    const res = await saveIssueTriage(projectId, { ...form, labelFilter: parseLabels(labelsRaw) });
    setSaving(false);
    switch (res.kind) {
      case "saved":
        onClose();
        return;
      case "invalid":
        setError({ message: res.message, fields: res.fields });
        return;
      case "denied":
        setError({
          message: "You no longer have permission to change this configuration.",
          fields: [],
        });
        return;
      case "unavailable":
        setError({
          message:
            res.status === 502
              ? "The triage model is unavailable right now — try again shortly."
              : "Issue auto-triage is not wired in this deployment yet.",
          fields: [],
        });
        return;
      default:
        setError({
          message: `Couldn't save (HTTP ${res.status}) — it will not have changed.`,
          fields: [],
        });
    }
  }, [form, canEdit, saving, projectId, labelsRaw, onClose]);

  const fieldHasError = (field: string) => error?.fields.includes(field) ?? false;

  return (
    <div
      className="gh-ra-scrim"
      data-testid="gh-it-scrim"
      onClick={onClose}
      role="presentation"
    >
      <div
        className="gh-ra-dialog"
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        data-testid="gh-issue-triage-dialog"
        onClick={(e) => e.stopPropagation()}
      >
        <header className="gh-ra-dialog__head">
          <h3 id={titleId} className="gh-ra-dialog__title">
            Issue auto-triage
          </h3>
          <button
            type="button"
            className="gh-ra-dialog__close"
            aria-label="Close"
            data-testid="gh-it-close-x"
            onClick={onClose}
          >
            ×
          </button>
        </header>

        <div className="gh-ra-dialog__body">
          {state.kind === "loading" && (
            <p className="muted" data-testid="gh-it-loading" aria-busy="true">
              Loading configuration…
            </p>
          )}

          {state.kind === "not-wired" && (
            <Honest
              testId="gh-it-not-wired"
              title="Not available yet"
              why="Issue auto-triage is not wired in this deployment — the config service isn't exposed here. This is expected until the operator ships it to this cluster."
            />
          )}
          {state.kind === "not-found" && (
            <Honest
              testId="gh-it-not-found"
              title="No repository linked"
              why="This project has no linked GitHub repository, or you cannot access its configuration."
            />
          )}
          {state.kind === "unauthenticated" && (
            <Honest
              testId="gh-it-unauth"
              title="Not signed in"
              why="Your session has expired — sign in again to configure issue auto-triage."
            />
          )}
          {state.kind === "error" && (
            <Honest
              testId="gh-it-error"
              title="Couldn't load configuration"
              why={`The read failed (HTTP ${state.status}) — close and reopen to retry.`}
            />
          )}

          {view && form && (
            <form
              className="gh-ra-form"
              data-testid="gh-it-form"
              onSubmit={(e) => {
                e.preventDefault();
                void submit();
              }}
            >
              {!canEdit && (
                <p className="gh-ra-readonly" role="status" data-testid="gh-it-readonly">
                  You can view this configuration but need contributor access to change it.
                </p>
              )}

              {/* (a) Enable toggle. */}
              <label className="gh-ra-field gh-ra-field--row" htmlFor={enableId}>
                <input
                  ref={firstFieldRef}
                  id={enableId}
                  type="checkbox"
                  data-testid="it-enable"
                  checked={form.enabled}
                  disabled={!canEdit}
                  onChange={(e) => patch({ enabled: e.target.checked })}
                />
                <span className="gh-ra-field__label">
                  Enable automated issue triage
                </span>
              </label>

              {/* (b) Triage agent dropdown — all team agents; the enabled⇒agent rule
                  is enforced by the server's 422, surfaced inline below. */}
              <label className="gh-ra-field" htmlFor={agentFieldId}>
                <span className="gh-ra-field__label">Triage agent</span>
                <select
                  id={agentFieldId}
                  data-testid="it-agent"
                  className={fieldHasError("triageAgentId") ? "gh-ra-invalid" : undefined}
                  aria-invalid={fieldHasError("triageAgentId") || undefined}
                  value={form.triageAgentId}
                  disabled={!canEdit}
                  onChange={(e) => patch({ triageAgentId: e.target.value })}
                >
                  <option value="">Select an agent…</option>
                  {/* Keep the stored agent selectable even if it isn't in the current
                      roster (renamed/removed) — never silently drop it. */}
                  {form.triageAgentId &&
                    !agents.some((a) => a.name === form.triageAgentId) && (
                      <option value={form.triageAgentId}>{form.triageAgentId}</option>
                    )}
                  {agents.map((a) => (
                    <option key={a.id} value={a.name}>
                      {agentOptionLabel(a)}
                    </option>
                  ))}
                </select>
                <span className="gh-ra-hint muted">
                  Any agent on this project&apos;s team. Required when triage is enabled.
                </span>
              </label>

              {/* (c) Label filter. */}
              <label className="gh-ra-field" htmlFor={labelsId}>
                <span className="gh-ra-field__label">Label filter</span>
                <input
                  id={labelsId}
                  type="text"
                  data-testid="it-labels"
                  value={labelsRaw}
                  disabled={!canEdit}
                  placeholder="bug, needs-triage"
                  onChange={(e) => {
                    setLabelsRaw(e.target.value);
                    patch({ labelFilter: parseLabels(e.target.value) });
                  }}
                />
                <span className="gh-ra-hint muted">
                  Comma-separated. Triage only issues carrying at least one of these
                  labels; leave empty to triage every new open issue.
                </span>
              </label>

              {/* (d) Only-unassigned toggle. */}
              <label className="gh-ra-field gh-ra-field--row" htmlFor={onlyUnassignedId}>
                <input
                  id={onlyUnassignedId}
                  type="checkbox"
                  data-testid="it-only-unassigned"
                  checked={form.onlyUnassigned}
                  disabled={!canEdit}
                  onChange={(e) => patch({ onlyUnassigned: e.target.checked })}
                />
                <span className="gh-ra-field__label">
                  Only triage issues with no GitHub assignee
                </span>
              </label>

              {view.enabledBy && (
                <p className="gh-ra-hint muted" data-testid="it-enabled-by">
                  Last enabled by <strong>{view.enabledBy}</strong>.
                </p>
              )}

              {error && (
                <p className="ksq-notice gh-ra-error" role="alert" data-testid="gh-it-save-error">
                  {error.message}
                </p>
              )}

              <footer className="gh-ra-dialog__foot">
                <button
                  type="button"
                  className="gh-btn"
                  data-testid="it-cancel"
                  onClick={onClose}
                >
                  Cancel
                </button>
                <button
                  type="submit"
                  className="gh-btn gh-btn--blue"
                  data-testid="it-save"
                  disabled={!canEdit || saving}
                >
                  {saving ? "Saving…" : "Save"}
                </button>
              </footer>
            </form>
          )}
        </div>
      </div>
    </div>
  );
}

function Honest({ testId, title, why }: { testId: string; title: string; why: string }) {
  return (
    <div className="gh-ra-honest" data-testid={testId}>
      <h4 className="gh-ra-honest__title">{title}</h4>
      <p className="muted">{why}</p>
    </div>
  );
}
