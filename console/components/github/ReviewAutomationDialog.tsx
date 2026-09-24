// components/github/ReviewAutomationDialog.tsx — ISI-4764 / ISI-4750 E2: the
// "Review automation" config dialog on the Pull Request Management header.
//
// A modal over the E1 sub-resource (fetchReviewAutomation / saveReviewAutomation
// → BFF app/api/projects/[id]/repo/review-automation → apiserver
// reviewautomation.go). It exposes the four config knobs the PRD approved:
//   (a) Enable toggle,
//   (b) Reviewer agent dropdown,
//   (c) Scope (team-authored vs all PRs),
//   (d) Trigger (on open / on new commits).
//
// HONESTY / authZ discipline (carried from the GitHub screen, ADR-0013 §D4):
//   - The apiserver is the wall. This dialog performs NO client-side authZ and
//     NO reviewer-eligibility pre-check — it gates the form on the server's
//     `canEdit` (read-only when false) and surfaces the write-time 422 (invalid
//     enum OR reviewer-not-code_review-capable) inline against the fields.
//   - The reviewer dropdown lists the code_review-capable team agents from the
//     D5 eligible-agents read surface (listEligibleReviewers → ISI-4779), which
//     the apiserver pre-filters via the SHARED pkg/reviewauto resolver. This is a
//     convenience pre-filter, NOT the wall: the E1 write-time 422 remains the
//     authoritative eligibility check (an agent that loses the capability between
//     load and save is still rejected on write).
//   - `enabledBy` (provenance) is server-stamped and shown read-only; it is
//     never sent back on write. Writing this config is inert until E3/E4 land —
//     the dialog says so honestly rather than implying live reviews.

"use client";

import { useCallback, useEffect, useId, useRef, useState } from "react";
import {
  fetchReviewAutomation,
  saveReviewAutomation,
  REVIEW_SCOPE_LABEL,
  REVIEW_TRIGGER_LABEL,
  type ReviewAutomationInput,
  type ReviewAutomationState,
  type ReviewScope,
  type ReviewTrigger,
} from "@/lib/github-status";
import { listEligibleReviewers, type AgentOption } from "@/lib/tickets/api";

const SCOPES: ReviewScope[] = ["team_authored", "all"];
const TRIGGERS: ReviewTrigger[] = ["on_open", "on_new_commits"];

/** The form's mutable state — the write-input subset, resolved from the view. */
type Form = ReviewAutomationInput;

export function ReviewAutomationDialog({
  projectId,
  onClose,
}: {
  projectId: string;
  onClose: () => void;
}) {
  const [state, setState] = useState<ReviewAutomationState>({ kind: "loading" });
  const [form, setForm] = useState<Form | null>(null);
  const [agents, setAgents] = useState<AgentOption[]>([]);
  const [saving, setSaving] = useState(false);
  // A field-level rejection surfaced from the write 422 (invalid enum or an
  // ineligible reviewer), or a deploy/authZ-level message.
  const [error, setError] = useState<{ message: string; fields: string[] } | null>(null);

  const titleId = useId();
  const enableId = useId();
  const reviewerId = useId();
  const scopeId = useId();
  const triggerId = useId();
  const firstFieldRef = useRef<HTMLInputElement>(null);

  // Load the current config; populate the form from the server's resolved
  // defaults. The dialog is honest about every terminal state the BFF relays.
  useEffect(() => {
    let alive = true;
    void fetchReviewAutomation(projectId).then((next) => {
      if (!alive) return;
      setState(next);
      if (next.kind === "ready") {
        const { enabled, reviewerAgentId, scope, trigger } = next.view;
        setForm({ enabled, reviewerAgentId, scope, trigger });
      }
    });
    return () => {
      alive = false;
    };
  }, [projectId]);

  // Populate the reviewer dropdown from the D5 eligible-agents pre-filter
  // (ISI-4779): only code_review-capable team agents. Best-effort — failure
  // degrades to an empty roster so the field still renders "Select an agent"
  // alone, and the write-time 422 stays the authoritative eligibility backstop.
  useEffect(() => {
    let alive = true;
    void listEligibleReviewers(projectId).then((list) => {
      if (alive) setAgents(list);
    });
    return () => {
      alive = false;
    };
  }, [projectId]);

  // Close on Escape (a11y for a modal dialog); focus the first field on open.
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
    const res = await saveReviewAutomation(projectId, form);
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
              ? "The reviewer model is unavailable right now — try again shortly."
              : "Review automation is not wired in this deployment yet.",
          fields: [],
        });
        return;
      default:
        setError({
          message: `Couldn't save (HTTP ${res.status}) — it will not have changed.`,
          fields: [],
        });
    }
  }, [form, canEdit, saving, projectId, onClose]);

  const fieldHasError = (field: string) => error?.fields.includes(field) ?? false;

  return (
    <div
      className="gh-ra-scrim"
      data-testid="gh-ra-scrim"
      onClick={onClose}
      role="presentation"
    >
      <div
        className="gh-ra-dialog"
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        data-testid="gh-review-automation-dialog"
        onClick={(e) => e.stopPropagation()}
      >
        <header className="gh-ra-dialog__head">
          <h3 id={titleId} className="gh-ra-dialog__title">
            Review automation
          </h3>
          <button
            type="button"
            className="gh-ra-dialog__close"
            aria-label="Close"
            data-testid="gh-ra-close-x"
            onClick={onClose}
          >
            ×
          </button>
        </header>

        <div className="gh-ra-dialog__body">
          {state.kind === "loading" && (
            <p className="muted" data-testid="gh-ra-loading" aria-busy="true">
              Loading configuration…
            </p>
          )}

          {state.kind === "not-wired" && (
            <Honest
              testId="gh-ra-not-wired"
              title="Not available yet"
              why="Review automation is not wired in this deployment — the config service isn't exposed here. This is expected until the operator ships it to this cluster."
            />
          )}
          {state.kind === "not-found" && (
            <Honest
              testId="gh-ra-not-found"
              title="No repository linked"
              why="This project has no linked GitHub repository, or you cannot access its configuration."
            />
          )}
          {state.kind === "unauthenticated" && (
            <Honest
              testId="gh-ra-unauth"
              title="Not signed in"
              why="Your session has expired — sign in again to configure review automation."
            />
          )}
          {state.kind === "error" && (
            <Honest
              testId="gh-ra-error"
              title="Couldn't load configuration"
              why={`The read failed (HTTP ${state.status}) — close and reopen to retry.`}
            />
          )}

          {view && form && (
            <form
              className="gh-ra-form"
              data-testid="gh-ra-form"
              onSubmit={(e) => {
                e.preventDefault();
                void submit();
              }}
            >
              {!canEdit && (
                <p className="gh-ra-readonly" role="status" data-testid="gh-ra-readonly">
                  You can view this configuration but need contributor access to change it.
                </p>
              )}

              {/* (a) Enable toggle. */}
              <label className="gh-ra-field gh-ra-field--row" htmlFor={enableId}>
                <input
                  ref={firstFieldRef}
                  id={enableId}
                  type="checkbox"
                  data-testid="ra-enable"
                  checked={form.enabled}
                  disabled={!canEdit}
                  onChange={(e) => patch({ enabled: e.target.checked })}
                />
                <span className="gh-ra-field__label">
                  Enable automated PR reviews
                </span>
              </label>

              {/* (b) Reviewer agent dropdown — all team agents; eligibility is
                  enforced by the server's 422 backstop, surfaced inline below. */}
              <label className="gh-ra-field" htmlFor={reviewerId}>
                <span className="gh-ra-field__label">Reviewer agent</span>
                <select
                  id={reviewerId}
                  data-testid="ra-reviewer"
                  className={fieldHasError("reviewerAgentId") ? "gh-ra-invalid" : undefined}
                  aria-invalid={fieldHasError("reviewerAgentId") || undefined}
                  value={form.reviewerAgentId}
                  disabled={!canEdit}
                  onChange={(e) => patch({ reviewerAgentId: e.target.value })}
                >
                  <option value="">Select an agent…</option>
                  {/* Keep the stored reviewer selectable even if it isn't in the
                      current roster (renamed/removed) — never silently drop it. */}
                  {form.reviewerAgentId &&
                    !agents.some((a) => a.name === form.reviewerAgentId) && (
                      <option value={form.reviewerAgentId}>{form.reviewerAgentId}</option>
                    )}
                  {agents.map((a) => (
                    <option key={a.id} value={a.name}>
                      {a.name}
                    </option>
                  ))}
                </select>
                <span className="gh-ra-hint muted">
                  The agent must have the <code>code_review</code> capability; the
                  server rejects an ineligible choice when you save.
                </span>
              </label>

              {/* (c) Scope. */}
              <fieldset className="gh-ra-field" data-testid="ra-scope" id={scopeId}>
                <legend className="gh-ra-field__label">Scope</legend>
                {SCOPES.map((s) => (
                  <label className="gh-ra-radio" key={s}>
                    <input
                      type="radio"
                      name="ra-scope"
                      value={s}
                      checked={form.scope === s}
                      disabled={!canEdit}
                      onChange={() => patch({ scope: s })}
                    />
                    <span>{REVIEW_SCOPE_LABEL[s]}</span>
                  </label>
                ))}
              </fieldset>

              {/* (d) Trigger. */}
              <fieldset className="gh-ra-field" data-testid="ra-trigger" id={triggerId}>
                <legend className="gh-ra-field__label">Trigger</legend>
                {TRIGGERS.map((t) => (
                  <label className="gh-ra-radio" key={t}>
                    <input
                      type="radio"
                      name="ra-trigger"
                      value={t}
                      checked={form.trigger === t}
                      disabled={!canEdit}
                      onChange={() => patch({ trigger: t })}
                    />
                    <span>{REVIEW_TRIGGER_LABEL[t]}</span>
                  </label>
                ))}
              </fieldset>

              {view.enabledBy && (
                <p className="gh-ra-hint muted" data-testid="ra-enabled-by">
                  Last enabled by <strong>{view.enabledBy}</strong>.
                </p>
              )}

              {error && (
                <p className="ksq-notice gh-ra-error" role="alert" data-testid="gh-ra-save-error">
                  {error.message}
                </p>
              )}

              <footer className="gh-ra-dialog__foot">
                <button
                  type="button"
                  className="gh-btn"
                  data-testid="ra-cancel"
                  onClick={onClose}
                >
                  Cancel
                </button>
                <button
                  type="submit"
                  className="gh-btn gh-btn--blue"
                  data-testid="ra-save"
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
