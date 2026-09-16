"use client";

// components/tickets/CreateTicketSheet.tsx — the create-ticket form (ISI-4399 S2,
// design ISI-4231 §2). A right slide-over sheet over a scrimmed Issues tab: chosen
// over a modal so the list context stays visible (operator surface, anti-hero).
//
// Fields are the ones the human CREATE contract accepts today (title / description /
// parent-as-sub-ticket, plus Priority / Work-mode / Labels since ISI-4409, see
// lib/tickets/createForm.ts); the payload is assembled exclusively by buildCreateBody
// so the wire body can't drift and an untouched optional stays absent. On success the sheet
// hands the server-assigned WorkItem back to the Issues screen for an optimistic
// insert (design §2 "optimistic row + link to detail") and closes.
//
// ASSIGN-TO (ISI-4501): assignment is NOT a create-body field — it is custody/dispatch
// based (ADR-0022 / ISI-4411). So the "Assign to" dropdown does NOT feed buildCreateBody;
// instead, on a successful 201 the sheet CHAINS POST /api/work-items/{id}/dispatch {agentId}
// which advances the item backlog→todo so operator Intake mints the Run with that agent.
// The agent identity dispatch matches is the agent NAME (Team.Spec.Agents), so the dropdown
// carries the name, not the UID. A dispatch failure is surfaced inline WITHOUT losing the
// created item — the ticket already exists in the backlog; only the assignment did not stick.
//
// Auth: the button that opens this is contributor+-gated in TicketsScreen, but the
// apiserver is the real wall — a viewer POST is refused 403 and surfaced here, never
// swallowed (fail-closed, §12.3).

import { useEffect, useId, useRef, useState } from "react";
import {
  ApiError,
  createWorkItem,
  dispatchWorkItem,
  listSquadAgents,
  type AgentOption,
} from "@/lib/tickets/api";
import {
  buildCreateBody,
  canCreate,
  EMPTY_CREATE_TICKET,
  type CreateTicketInput,
} from "@/lib/tickets/createForm";
import {
  PRIORITY_LABELS,
  WORK_ITEM_PRIORITIES,
  WORK_ITEM_MODES,
  WORK_MODE_LABELS,
  type WorkItem,
} from "@/lib/tickets/types";

export interface CreateTicketSheetProps {
  projectId: string;
  /** Root items offered as parent candidates for the sub-ticket search-select. */
  parents: WorkItem[];
  /** Receives the server-assigned item on 201 so the list inserts it optimistically. */
  onCreated: (item: WorkItem) => void;
  onClose: () => void;
}

function errorMessage(err: unknown): string {
  if (err instanceof ApiError) {
    switch (err.status) {
      case 400:
        return "The server rejected the ticket — check the title and try again.";
      case 403:
        return "You don't have permission to create tickets in this project.";
      case 404:
        return "This project is no longer available.";
      case 501:
        return "Ticket creation isn't hosted on this deployment yet.";
      default:
        return `Could not create the ticket (HTTP ${err.status}).`;
    }
  }
  return "Could not create the ticket — network error.";
}

// The create SUCCEEDED — only the follow-on dispatch failed. Every message keeps
// that distinction explicit (the ticket is safe in the backlog) so the human
// retries the assignment rather than the whole create (ISI-4501).
function dispatchErrorMessage(err: unknown): string {
  const tail =
    " The ticket was created and is in the backlog — retry, or assign it from its detail.";
  if (err instanceof ApiError) {
    switch (err.status) {
      case 403:
        return "That agent can't be assigned here (not a member of this project's team)." + tail;
      case 404:
        return "The ticket could not be found to assign the agent." + tail;
      case 409:
        return "The ticket already moved out of the backlog, so it can't be dispatched." + tail;
      case 501:
        return "Assigning agents isn't hosted on this deployment yet." + tail;
      default:
        return `Could not assign the agent (HTTP ${err.status}).` + tail;
    }
  }
  return "Could not assign the agent — network error." + tail;
}

export function CreateTicketSheet({
  projectId,
  parents,
  onCreated,
  onClose,
}: CreateTicketSheetProps) {
  const [input, setInput] = useState<CreateTicketInput>(EMPTY_CREATE_TICKET);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  // "Assign to" is held OUTSIDE CreateTicketInput on purpose: it is dispatched
  // after create, never posted in the create body (buildCreateBody stays
  // assignee-free, ISI-4501). Value is the agent NAME dispatch matches on.
  const [agents, setAgents] = useState<AgentOption[]>([]);
  const [assigneeName, setAssigneeName] = useState<string>("");
  // Once created, we remember the item so a dispatch retry re-uses it instead of
  // creating a duplicate ticket (the create is idempotent-by-hand this way).
  const [created, setCreated] = useState<WorkItem | null>(null);
  const titleRef = useRef<HTMLInputElement>(null);
  const titleId = useId();
  const descId = useId();
  const parentId = useId();
  const priorityId = useId();
  const workModeId = useId();
  const assigneeId = useId();
  const labelsId = useId();

  // Focus the title on open, and close on Escape (a11y for a slide-over dialog).
  useEffect(() => {
    titleRef.current?.focus();
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [onClose]);

  // Populate the agent dropdown (best-effort; failure degrades to Unassigned-only).
  useEffect(() => {
    let alive = true;
    void listSquadAgents().then((list) => {
      if (alive) setAgents(list);
    });
    return () => {
      alive = false;
    };
  }, []);

  const disabled = submitting || !canCreate(input);

  const submit = async () => {
    if (!canCreate(input) || submitting) return;
    setSubmitting(true);
    setError(null);

    // (1) Create — but only if we haven't already (a dispatch retry re-uses it).
    let item = created;
    if (!item) {
      try {
        item = await createWorkItem(projectId, buildCreateBody(input));
      } catch (err) {
        setError(errorMessage(err));
        setSubmitting(false);
        return;
      }
      setCreated(item);
      // Hand the backlog item to the list immediately so it is never lost even if
      // the follow-on dispatch fails (onCreated upserts by id, so a later
      // assigned-row hand-back replaces this one).
      onCreated(item);
    }

    // (2) If an agent was picked, chain the dispatch (assignment == start).
    if (assigneeName) {
      try {
        const res = await dispatchWorkItem(item.id, assigneeName);
        onCreated({
          ...item,
          assignee: res.requestedAgent,
          state: res.toState as WorkItem["state"],
        });
      } catch (err) {
        setError(dispatchErrorMessage(err));
        setSubmitting(false);
        return;
      }
    }

    onClose();
  };

  return (
    <div className="ksq-sheet-scrim" data-testid="create-ticket-scrim" onClick={onClose}>
      <aside
        className="ksq-sheet"
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        data-testid="create-ticket-sheet"
        // Clicks inside the sheet must not fall through to the scrim's close.
        onClick={(e) => e.stopPropagation()}
      >
        <form
          className="ksq-sheet__form"
          onSubmit={(e) => {
            e.preventDefault();
            void submit();
          }}
        >
          <header className="ksq-sheet__head">
            <h2 id={titleId} style={{ margin: 0 }}>
              New issue
            </h2>
            <button
              type="button"
              className="ksq-sheet__close"
              aria-label="Close"
              data-testid="create-ticket-cancel-x"
              onClick={onClose}
            >
              ×
            </button>
          </header>

          <div className="ksq-sheet__body">
            <label className="ksq-field">
              <span className="ksq-field__label">
                Title <span aria-hidden="true">*</span>
              </span>
              <input
                ref={titleRef}
                type="text"
                required
                data-testid="create-ticket-title"
                aria-label="Title"
                value={input.title}
                onChange={(e) => setInput((s) => ({ ...s, title: e.target.value }))}
              />
            </label>

            <label className="ksq-field" htmlFor={descId}>
              <span className="ksq-field__label">Description</span>
              <textarea
                id={descId}
                rows={8}
                data-testid="create-ticket-description"
                aria-label="Description"
                placeholder="Describe the work… Markdown supported."
                value={input.body}
                onChange={(e) => setInput((s) => ({ ...s, body: e.target.value }))}
              />
              <span className="ksq-field__hint muted">Markdown supported</span>
            </label>

            <label className="ksq-field" htmlFor={parentId}>
              <span className="ksq-field__label">Parent (optional)</span>
              <select
                id={parentId}
                data-testid="create-ticket-parent"
                aria-label="Parent issue"
                value={input.parentId ?? ""}
                onChange={(e) =>
                  setInput((s) => ({ ...s, parentId: e.target.value || null }))
                }
              >
                <option value="">None — a top-level issue</option>
                {parents.map((p) => (
                  <option key={p.id} value={p.id}>
                    {p.title}
                  </option>
                ))}
              </select>
              <span className="ksq-field__hint muted">
                Pick a parent to file this as a sub-ticket.
              </span>
            </label>

            <label className="ksq-field" htmlFor={priorityId}>
              <span className="ksq-field__label">Priority</span>
              <select
                id={priorityId}
                data-testid="create-ticket-priority"
                aria-label="Priority"
                value={input.priority}
                onChange={(e) =>
                  setInput((s) => ({
                    ...s,
                    priority: e.target.value as CreateTicketInput["priority"],
                  }))
                }
              >
                <option value="">No priority</option>
                {WORK_ITEM_PRIORITIES.map((p) => (
                  <option key={p} value={p}>
                    {PRIORITY_LABELS[p]}
                  </option>
                ))}
              </select>
            </label>

            <label className="ksq-field" htmlFor={workModeId}>
              <span className="ksq-field__label">Work mode</span>
              <select
                id={workModeId}
                data-testid="create-ticket-workmode"
                aria-label="Work mode"
                value={input.workMode}
                onChange={(e) =>
                  setInput((s) => ({
                    ...s,
                    workMode: e.target.value as CreateTicketInput["workMode"],
                  }))
                }
              >
                <option value="">Default (standard)</option>
                {WORK_ITEM_MODES.map((m) => (
                  <option key={m} value={m}>
                    {WORK_MODE_LABELS[m]}
                  </option>
                ))}
              </select>
            </label>

            <label className="ksq-field" htmlFor={assigneeId}>
              <span className="ksq-field__label">Assign to (optional)</span>
              <select
                id={assigneeId}
                data-testid="create-ticket-assignee"
                aria-label="Assign to agent"
                value={assigneeName}
                onChange={(e) => setAssigneeName(e.target.value)}
              >
                <option value="">Unassigned</option>
                {agents.map((a) => (
                  <option key={a.id} value={a.name}>
                    {a.name}
                  </option>
                ))}
              </select>
              <span className="ksq-field__hint muted">
                Assigning dispatches the agent to start this ticket. Leave
                unassigned to keep it in the backlog.
              </span>
            </label>

            <label className="ksq-field" htmlFor={labelsId}>
              <span className="ksq-field__label">Labels</span>
              <input
                id={labelsId}
                type="text"
                data-testid="create-ticket-labels"
                aria-label="Labels"
                placeholder="e.g. backend, urgent-fix"
                value={input.labels}
                onChange={(e) =>
                  setInput((s) => ({ ...s, labels: e.target.value }))
                }
              />
              <span className="ksq-field__hint muted">
                Comma-separated. Freeform chips — new labels are created on the fly.
              </span>
            </label>

            {error && (
              <p className="ksq-notice" role="alert" data-testid="create-ticket-error">
                {error}
              </p>
            )}
          </div>

          <footer className="ksq-sheet__foot">
            <button
              type="button"
              className="ksq-btn"
              data-testid="create-ticket-cancel"
              onClick={onClose}
            >
              Cancel
            </button>
            <button
              type="submit"
              className="ksq-btn ksq-btn--primary"
              data-testid="create-ticket-submit"
              disabled={disabled}
            >
              {submitting
                ? created
                  ? "Assigning…"
                  : "Creating…"
                : created
                  ? "Retry assign"
                  : "Create ticket"}
            </button>
          </footer>
        </form>
      </aside>
    </div>
  );
}

export default CreateTicketSheet;
