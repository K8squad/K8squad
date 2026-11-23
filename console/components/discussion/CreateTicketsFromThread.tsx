"use client";

// components/discussion/CreateTicketsFromThread.tsx — the human-initiated
// "create tickets from this thread" sheet (ISI-7710 / ISI-7681 P1; UX flow
// spec ISI-7709 §2-§5). Modeled on tickets/CreateTicketSheet.tsx (same
// slide-over + scrim + fail-closed error surface) with the thread-scoped
// deltas only:
//
//   - header copy ("Create ticket from thread");
//   - the POST goes to the thread-scoped work-items endpoint (ISI-7708
//     contract) through the DiscussionClient, carrying the digest's
//     sourceMessageIds so every created item keeps its provenance backlink;
//   - fields are PRE-SEEDED from the thread digest (title + a Markdown digest
//     of the recent messages) and stay fully editable — the human confirms,
//     nothing auto-files.
//
// Phase-1 is single-ticket-per-pass (UX spec §2): the wire accepts an array,
// the sheet drafts one; multi-ticket is Phase-2 with no contract change.
// Human-only, no auto-dispatch — assignment/state stay separate explicit
// actions on the created item, exactly like CreateTicketSheet.
//
// States (UX spec §5): errors keep the sheet OPEN with the draft intact; the
// duplicate heads-up is non-blocking (a thread may legitimately yield several
// tickets); success shows the created item(s) with links and stays until
// dismissed so the links are never lost behind a close.

import { useEffect, useId, useRef, useState } from "react";
import type { DiscussionClient } from "@/lib/discussion/api";
import { DiscussionApiError } from "@/lib/discussion/api";
import type {
  CreatedThreadTicket,
  ThreadTicketDraft,
} from "@/lib/discussion/types";
import type { ThreadDigest } from "@/lib/discussion/threadDigest";
import {
  PRIORITY_LABELS,
  WORK_ITEM_PRIORITIES,
} from "@/lib/tickets/types";

export interface CreateTicketsFromThreadProps {
  projectId: string;
  threadId: string;
  client: DiscussionClient;
  /** Pre-seed (title + digest body + provenance ids), UX spec §3. */
  seed: ThreadDigest;
  /** Titles of tickets already linked to this thread — non-blocking warn. */
  existingTitles: readonly string[];
  /** Receives the created items so the room can post the linked post-back. */
  onCreated: (created: CreatedThreadTicket[]) => void;
  onClose: () => void;
}

// Error mapping (UX spec §5): reuse the CreateTicketSheet classes, plus the
// thread-scoped 404 (the discussion itself went away). The sheet stays open
// and the draft intact on every branch.
function errorMessage(err: unknown): string {
  if (err instanceof DiscussionApiError) {
    switch (err.status) {
      case 400:
        return "The server rejected the ticket — check the title and try again.";
      case 403:
        return "You don't have permission to create tickets in this project.";
      case 404:
        return "This discussion is no longer available.";
      case 501:
        return "Ticket creation isn't hosted on this deployment yet.";
      default:
        return "Could not create the ticket (HTTP " + err.status + ").";
    }
  }
  return "Could not create the ticket — network error. Your draft is intact.";
}

export function CreateTicketsFromThread({
  projectId,
  threadId,
  client,
  seed,
  existingTitles,
  onCreated,
  onClose,
}: CreateTicketsFromThreadProps) {
  const [title, setTitle] = useState(seed.title);
  const [body, setBody] = useState(seed.body);
  const [priority, setPriority] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [created, setCreated] = useState<CreatedThreadTicket[] | null>(null);
  const titleRef = useRef<HTMLInputElement>(null);
  const titleId = useId();
  const descId = useId();
  const priorityId = useId();

  // Focus the title on open, close on Escape (a11y, like CreateTicketSheet).
  useEffect(() => {
    titleRef.current?.focus();
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [onClose]);

  const disabled = submitting || created !== null || title.trim().length === 0;

  const submit = async () => {
    if (disabled) return;
    // Optional capability on older stub clients — the documented degrade
    // (mirrors the 501 copy; the apiserver stays the real wall).
    if (typeof client.createThreadTickets !== "function") {
      setError("Ticket creation isn't hosted on this deployment yet.");
      return;
    }
    setSubmitting(true);
    setError(null);
    // Wire body per the ISI-7708 contract: title required; body/priority only
    // when set (an untouched optional stays absent); the digest's message ids
    // ride the draft so the backend stamps the provenance backlinks.
    const draft: ThreadTicketDraft = { title: title.trim() };
    const trimmedBody = body.trim();
    if (trimmedBody) draft.body = trimmedBody;
    if (priority) draft.priority = priority;
    if (seed.sourceMessageIds.length > 0) {
      draft.sourceMessageIds = seed.sourceMessageIds;
    }
    try {
      const res = await client.createThreadTickets(projectId, threadId, [
        draft,
      ]);
      setCreated(res.created);
      onCreated(res.created);
    } catch (err) {
      setError(errorMessage(err)); // sheet stays open, draft intact (UX §5)
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <div
      className="ksq-sheet-scrim"
      data-testid="thread-tickets-scrim"
      onClick={onClose}
    >
      <aside
        className="ksq-sheet"
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        data-testid="thread-tickets-sheet"
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
              Create ticket from thread
            </h2>
            <button
              type="button"
              className="ksq-sheet__close"
              aria-label="Close"
              data-testid="thread-tickets-cancel-x"
              onClick={onClose}
            >
              ×
            </button>
          </header>

          <div className="ksq-sheet__body">
            {created ? (
              <div data-testid="thread-tickets-created">
                <p className="ksq-notice" role="status">
                  ✓ Ticket created — filed in the backlog and linked back to
                  this thread.
                </p>
                <ul>
                  {created.map((t) => (
                    <li key={t.id}>
                      <a
                        href={t.url}
                        data-testid="thread-tickets-created-link"
                      >
                        {t.title}
                      </a>
                      {t.labels.length > 0 ? (
                        <span className="muted"> · {t.labels.join(" · ")}</span>
                      ) : null}
                    </li>
                  ))}
                </ul>
              </div>
            ) : (
              <>
                <label className="ksq-field">
                  <span className="ksq-field__label">
                    Title <span aria-hidden="true">*</span>
                  </span>
                  <input
                    ref={titleRef}
                    type="text"
                    required
                    data-testid="thread-tickets-title"
                    aria-label="Title"
                    value={title}
                    onChange={(e) => setTitle(e.target.value)}
                  />
                </label>

                <label className="ksq-field" htmlFor={descId}>
                  <span className="ksq-field__label">Description</span>
                  <textarea
                    id={descId}
                    rows={8}
                    data-testid="thread-tickets-description"
                    aria-label="Description"
                    placeholder="Describe the work… Markdown supported."
                    value={body}
                    onChange={(e) => setBody(e.target.value)}
                  />
                  <span className="ksq-field__hint muted">
                    Pre-filled from this thread&#39;s recent messages — edit freely.
                  </span>
                </label>

                <label className="ksq-field" htmlFor={priorityId}>
                  <span className="ksq-field__label">Priority</span>
                  <select
                    id={priorityId}
                    data-testid="thread-tickets-priority"
                    aria-label="Priority"
                    value={priority}
                    onChange={(e) => setPriority(e.target.value)}
                  >
                    <option value="">No priority</option>
                    {WORK_ITEM_PRIORITIES.map((p) => (
                      <option key={p} value={p}>
                        {PRIORITY_LABELS[p]}
                      </option>
                    ))}
                  </select>
                </label>

                {existingTitles.length > 0 ? (
                  <p
                    className="ksq-notice"
                    data-testid="thread-tickets-dup-warn"
                  >
                    Heads-up:{" "}
                    {existingTitles.length === 1
                      ? "a ticket was"
                      : existingTitles.length + " tickets were"}{" "}
                    already created from this thread (
                    {existingTitles.slice(0, 3).join(", ")}
                    {existingTitles.length > 3 ? ", …" : ""}). A thread may
                    legitimately yield several — this is just a heads-up.
                  </p>
                ) : null}

                {error ? (
                  <p
                    className="ksq-notice"
                    role="alert"
                    data-testid="thread-tickets-error"
                  >
                    {error}
                  </p>
                ) : null}
              </>
            )}
          </div>

          <footer className="ksq-sheet__foot">
            <button
              type="button"
              className="ksq-btn"
              data-testid="thread-tickets-cancel"
              onClick={onClose}
            >
              {created ? "Done" : "Cancel"}
            </button>
            {created ? null : (
              <button
                type="submit"
                className="ksq-btn ksq-btn--primary"
                data-testid="thread-tickets-submit"
                disabled={disabled}
              >
                {submitting ? "Creating…" : "Create ticket"}
              </button>
            )}
          </footer>
        </form>
      </aside>
    </div>
  );
}

export default CreateTicketsFromThread;
