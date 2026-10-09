// components/github/CiFailureDialog.tsx — ISI-5595 WS-E: the "CI-failure triage"
// config dialog on the CI/CD Pipeline Status panel header.
//
// A near-clone of ReviewAutomationDialog (ISI-4764) for visual + behavioural
// parity: a modal over the ci-failure sub-resource (fetchCiFailure / saveCiFailure
// → BFF app/api/projects/[id]/repo/ci-automation → apiserver cifailure.go). It
// exposes the WS-C config knobs:
//   (a) Enable toggle,
//   (b) Triage agent dropdown (any team agent),
//   (c) Branch scope (restrict to these head branches),
//   (d) Conclusion selector (which terminal conclusions count as a failure).
//
// HONESTY / authZ discipline matches ReviewAutomationDialog: the apiserver is the
// wall; the form gates on `canEdit` and surfaces the write-time 422 inline.

"use client";

import { useCallback, useEffect, useId, useRef, useState } from "react";
import {
  fetchCiFailure,
  saveCiFailure,
  CI_FAILURE_CONCLUSIONS,
  CI_FAILURE_CONCLUSION_LABEL,
  type CiFailureConclusion,
  type CiFailureInput,
  type CiFailureState,
} from "@/lib/github-status";
import { agentOptionLabel, listSquadAgents, type AgentOption } from "@/lib/tickets/api";

/** The form's mutable state — the write-input subset, resolved from the view. */
type Form = CiFailureInput;

/** Parse the comma/newline-separated branch textbox into a trimmed, de-duped list. */
function parseBranches(raw: string): string[] {
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

/** Toggle one conclusion in the current set, preserving the canonical order. */
function toggleConclusion(
  current: CiFailureConclusion[],
  c: CiFailureConclusion,
  on: boolean,
): CiFailureConclusion[] {
  const set = new Set(current);
  if (on) set.add(c);
  else set.delete(c);
  return CI_FAILURE_CONCLUSIONS.filter((x) => set.has(x));
}

export function CiFailureDialog({
  projectId,
  onClose,
}: {
  projectId: string;
  onClose: () => void;
}) {
  const [state, setState] = useState<CiFailureState>({ kind: "loading" });
  const [form, setForm] = useState<Form | null>(null);
  const [branchesRaw, setBranchesRaw] = useState("");
  const [agents, setAgents] = useState<AgentOption[]>([]);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<{ message: string; fields: string[] } | null>(null);

  const titleId = useId();
  const enableId = useId();
  const agentFieldId = useId();
  const branchesId = useId();
  const conclusionsId = useId();
  const firstFieldRef = useRef<HTMLInputElement>(null);

  useEffect(() => {
    let alive = true;
    void fetchCiFailure(projectId).then((next) => {
      if (!alive) return;
      setState(next);
      if (next.kind === "ready") {
        const { enabled, agentId, branchFilter, conclusions } = next.view;
        setForm({ enabled, agentId, branchFilter, conclusions });
        setBranchesRaw(branchFilter.join(", "));
      }
    });
    return () => {
      alive = false;
    };
  }, [projectId]);

  useEffect(() => {
    let alive = true;
    void listSquadAgents().then((list) => {
      if (alive) setAgents(list);
    });
    return () => {
      alive = false;
    };
  }, []);

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
    const res = await saveCiFailure(projectId, { ...form, branchFilter: parseBranches(branchesRaw) });
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
              : "CI-failure triage is not wired in this deployment yet.",
          fields: [],
        });
        return;
      default:
        setError({
          message: `Couldn't save (HTTP ${res.status}) — it will not have changed.`,
          fields: [],
        });
    }
  }, [form, canEdit, saving, projectId, branchesRaw, onClose]);

  const fieldHasError = (field: string) => error?.fields.includes(field) ?? false;

  return (
    <div
      className="gh-ra-scrim"
      data-testid="gh-cf-scrim"
      onClick={onClose}
      role="presentation"
    >
      <div
        className="gh-ra-dialog"
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        data-testid="gh-ci-failure-dialog"
        onClick={(e) => e.stopPropagation()}
      >
        <header className="gh-ra-dialog__head">
          <h3 id={titleId} className="gh-ra-dialog__title">
            CI-failure triage
          </h3>
          <button
            type="button"
            className="gh-ra-dialog__close"
            aria-label="Close"
            data-testid="gh-cf-close-x"
            onClick={onClose}
          >
            ×
          </button>
        </header>

        <div className="gh-ra-dialog__body">
          {state.kind === "loading" && (
            <p className="muted" data-testid="gh-cf-loading" aria-busy="true">
              Loading configuration…
            </p>
          )}

          {state.kind === "not-wired" && (
            <Honest
              testId="gh-cf-not-wired"
              title="Not available yet"
              why="CI-failure triage is not wired in this deployment — the config service isn't exposed here. This is expected until the operator ships it to this cluster."
            />
          )}
          {state.kind === "not-found" && (
            <Honest
              testId="gh-cf-not-found"
              title="No repository linked"
              why="This project has no linked GitHub repository, or you cannot access its configuration."
            />
          )}
          {state.kind === "unauthenticated" && (
            <Honest
              testId="gh-cf-unauth"
              title="Not signed in"
              why="Your session has expired — sign in again to configure CI-failure triage."
            />
          )}
          {state.kind === "error" && (
            <Honest
              testId="gh-cf-error"
              title="Couldn't load configuration"
              why={`The read failed (HTTP ${state.status}) — close and reopen to retry.`}
            />
          )}

          {view && form && (
            <form
              className="gh-ra-form"
              data-testid="gh-cf-form"
              onSubmit={(e) => {
                e.preventDefault();
                void submit();
              }}
            >
              {!canEdit && (
                <p className="gh-ra-readonly" role="status" data-testid="gh-cf-readonly">
                  You can view this configuration but need contributor access to change it.
                </p>
              )}

              {/* (a) Enable toggle. */}
              <label className="gh-ra-field gh-ra-field--row" htmlFor={enableId}>
                <input
                  ref={firstFieldRef}
                  id={enableId}
                  type="checkbox"
                  data-testid="cf-enable"
                  checked={form.enabled}
                  disabled={!canEdit}
                  onChange={(e) => patch({ enabled: e.target.checked })}
                />
                <span className="gh-ra-field__label">
                  Enable automated CI-failure triage
                </span>
              </label>

              {/* (b) Triage agent dropdown. */}
              <label className="gh-ra-field" htmlFor={agentFieldId}>
                <span className="gh-ra-field__label">Triage agent</span>
                <select
                  id={agentFieldId}
                  data-testid="cf-agent"
                  className={fieldHasError("agentId") ? "gh-ra-invalid" : undefined}
                  aria-invalid={fieldHasError("agentId") || undefined}
                  value={form.agentId}
                  disabled={!canEdit}
                  onChange={(e) => patch({ agentId: e.target.value })}
                >
                  <option value="">Select an agent…</option>
                  {form.agentId &&
                    !agents.some((a) => a.name === form.agentId) && (
                      <option value={form.agentId}>{form.agentId}</option>
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

              {/* (c) Branch scope. */}
              <label className="gh-ra-field" htmlFor={branchesId}>
                <span className="gh-ra-field__label">Branch scope</span>
                <input
                  id={branchesId}
                  type="text"
                  data-testid="cf-branches"
                  value={branchesRaw}
                  disabled={!canEdit}
                  placeholder="main, release/*"
                  onChange={(e) => {
                    setBranchesRaw(e.target.value);
                    patch({ branchFilter: parseBranches(e.target.value) });
                  }}
                />
                <span className="gh-ra-hint muted">
                  Comma-separated head branches. Leave empty to triage every mirrored
                  check-run ref (default branch + open-PR heads).
                </span>
              </label>

              {/* (d) Conclusion selector. */}
              <fieldset className="gh-ra-field" data-testid="cf-conclusions" id={conclusionsId}>
                <legend className="gh-ra-field__label">Qualifying conclusions</legend>
                {CI_FAILURE_CONCLUSIONS.map((c) => (
                  <label className="gh-ra-radio" key={c}>
                    <input
                      type="checkbox"
                      name="cf-conclusion"
                      data-testid={`cf-conclusion-${c}`}
                      value={c}
                      checked={form.conclusions.includes(c)}
                      disabled={!canEdit}
                      onChange={(e) =>
                        patch({ conclusions: toggleConclusion(form.conclusions, c, e.target.checked) })
                      }
                    />
                    <span>{CI_FAILURE_CONCLUSION_LABEL[c]}</span>
                  </label>
                ))}
                <span className="gh-ra-hint muted">
                  Which terminal check-run conclusions count as a failure. Defaults to
                  Failed when none are selected.
                </span>
              </fieldset>

              {view.enabledBy && (
                <p className="gh-ra-hint muted" data-testid="cf-enabled-by">
                  Last enabled by <strong>{view.enabledBy}</strong>.
                </p>
              )}

              {error && (
                <p className="ksq-notice gh-ra-error" role="alert" data-testid="gh-cf-save-error">
                  {error.message}
                </p>
              )}

              <footer className="gh-ra-dialog__foot">
                <button
                  type="button"
                  className="gh-btn"
                  data-testid="cf-cancel"
                  onClick={onClose}
                >
                  Cancel
                </button>
                <button
                  type="submit"
                  className="gh-btn gh-btn--blue"
                  data-testid="cf-save"
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
