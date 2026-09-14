"use client";

// components/tickets/TicketDetail.tsx — the ticket-detail screen (ISI-4399 S3,
// design ISI-4231 §3): a full workspace page showing one ticket's header,
// description, sub-tickets, and the agent Activity thread, with a Properties +
// "Agent run & trace" sidebar. "Paperclip parity" — read the ticket, watch its
// sub-tickets, and read the agent conversation, without a human relay.
//
// Everything here is a REPROJECTION of two reads the console already owns:
//   • GET /api/work-items/{id}  → the thread (comments + status history + change
//     refs), via fetchWorkItemThread.
//   • GET /api/projects/{id}/work-items?parentId={id} → the sub-ticket rows.
// Each read is its own async boundary so a slow/absent children read never blanks
// the ticket body (per-panel isolation, S1 discipline).
//
// WRITE GAP (honest, FR-I3): the design's composer ("Comment" / "Comment &
// assign") needs a HUMAN work-item comment endpoint. Only the agent task-io
// post-comment path exists today (run-token authed), so the composer is rendered
// DISABLED with a note and the write is split to a backend follow-up — the
// console never posts to an endpoint that isn't there. The Dynatrace trace
// deep-link (S4) is likewise deferred to the OBS contract child.

import { useEffect, useState } from "react";
import { ApiError, listWorkItems } from "@/lib/tickets/api";
import {
  buildActivity,
  fetchWorkItemThread,
  subTicketProgress,
  type ActivityItem,
  type NormalizedThread,
} from "@/lib/tickets/thread";
import { STATE_LABELS, type WorkItem, type WorkItemState } from "@/lib/tickets/types";

type ThreadState =
  | { kind: "loading" }
  | { kind: "not-available" } // 501 store-less / 404 existence-hiding
  | { kind: "error"; status: number }
  | { kind: "ready"; thread: NormalizedThread };

type ChildrenState =
  | { kind: "loading" }
  | { kind: "not-available" }
  | { kind: "error"; status: number }
  | { kind: "ready"; items: WorkItem[] };

function stateLabel(state: string): string {
  return STATE_LABELS[state as WorkItemState] ?? state;
}

function shortId(id: string): string {
  // The board renders a short mono id; the full id stays in the title attr.
  return id.length > 10 ? id.slice(0, 8) : id;
}

function fmt(ts: string): string {
  const t = Date.parse(ts);
  return Number.isNaN(t) ? "just now" : new Date(t).toLocaleString();
}

function useThread(workItemId: string): ThreadState {
  const [state, setState] = useState<ThreadState>({ kind: "loading" });
  useEffect(() => {
    let alive = true;
    setState({ kind: "loading" });
    fetchWorkItemThread(workItemId)
      .then((thread) => alive && setState({ kind: "ready", thread }))
      .catch((err: unknown) => {
        if (!alive) return;
        const status = err instanceof ApiError ? err.status : 0;
        setState(
          status === 501 || status === 404
            ? { kind: "not-available" }
            : { kind: "error", status },
        );
      });
    return () => {
      alive = false;
    };
  }, [workItemId]);
  return state;
}

function useChildren(projectId: string, parentId: string): ChildrenState {
  const [state, setState] = useState<ChildrenState>({ kind: "loading" });
  useEffect(() => {
    let alive = true;
    setState({ kind: "loading" });
    listWorkItems(projectId, { parentId })
      .then((items) => alive && setState({ kind: "ready", items }))
      .catch((err: unknown) => {
        if (!alive) return;
        const status = err instanceof ApiError ? err.status : 0;
        setState(
          status === 501 || status === 404
            ? { kind: "not-available" }
            : { kind: "error", status },
        );
      });
    return () => {
      alive = false;
    };
  }, [projectId, parentId]);
  return state;
}

function StatusChip({ state }: { state: string }) {
  return (
    <span className="ksq-chip ksq-chip--state" data-testid="detail-status">
      {stateLabel(state)}
    </span>
  );
}

function ActivityRow({ item }: { item: ActivityItem }) {
  if (item.kind === "event" && item.event) {
    const e = item.event;
    return (
      <li className="ksq-activity ksq-activity--event" data-testid="activity-event">
        <span className="ksq-activity__dot" aria-hidden="true" />
        <span className="muted">
          {e.principal || "someone"} moved {stateLabel(e.fromState)} →{" "}
          {stateLabel(e.toState)}
        </span>
        <time className="ksq-ticket-id">{fmt(item.at)}</time>
      </li>
    );
  }
  if (item.kind === "change" && item.change) {
    const c = item.change;
    const isPr = c.kind === "pull_request";
    return (
      <li className="ksq-activity ksq-activity--change" data-testid="activity-change">
        <span className="ksq-chip ksq-chip--prov">{isPr ? "PR" : "commit"}</span>
        {isPr ? (
          <a href={c.ref} target="_blank" rel="noopener noreferrer">
            {c.summary || c.ref}
          </a>
        ) : (
          <code className="ksq-ticket-id" title={c.ref}>
            {c.summary || shortId(c.ref)}
          </code>
        )}
        <span className="muted">· {c.author}</span>
        <time className="ksq-ticket-id">{fmt(item.at)}</time>
      </li>
    );
  }
  if (item.kind === "comment" && item.comment) {
    return (
      <li className="ksq-activity ksq-activity--comment" data-testid="activity-comment">
        <div className="ksq-activity__head">
          <strong>{item.comment.author || "unknown"}</strong>
          <span
            className="ksq-chip"
            data-role={item.authorKind}
            data-testid="activity-role"
          >
            {item.authorKind}
          </span>
          <time className="ksq-ticket-id">{fmt(item.at)}</time>
        </div>
        <p className="ksq-activity__body">{item.comment.body}</p>
      </li>
    );
  }
  return null;
}

export function TicketDetail({
  projectId,
  workItemId,
}: {
  projectId: string;
  workItemId: string;
}) {
  const thread = useThread(workItemId);
  const children = useChildren(projectId, workItemId);
  const issuesHref = `/projects/${encodeURIComponent(projectId)}/issues`;

  return (
    <div className="ksq-ticket-detail" data-testid="ticket-detail">
      <nav className="ksq-ticket-detail__crumb">
        <a href={issuesHref} data-testid="detail-back">
          ← Issues
        </a>
      </nav>

      {thread.kind === "loading" ? (
        <p className="ksq-empty-hint" data-testid="detail-loading">
          Loading ticket…
        </p>
      ) : thread.kind === "not-available" ? (
        <div className="ksq-empty-state" data-testid="detail-unavailable">
          <p className="ksq-empty-state__title">This ticket isn’t available.</p>
          <p className="muted">
            It may not exist, or the ticket read model isn’t hosted on this
            deployment yet — nothing is fabricated here (FR-I3).
          </p>
        </div>
      ) : thread.kind === "error" ? (
        <div className="ksq-empty-state" data-testid="detail-error">
          <p className="ksq-empty-state__title">Couldn’t load this ticket.</p>
          <p className="muted">HTTP {thread.status || "network error"}.</p>
        </div>
      ) : (
        <TicketBody
          projectId={projectId}
          thread={thread.thread}
          childrenState={children}
        />
      )}
    </div>
  );
}

function TicketBody({
  projectId,
  thread,
  childrenState,
}: {
  projectId: string;
  thread: NormalizedThread;
  childrenState: ChildrenState;
}) {
  const activity = buildActivity(thread);
  const issuesHref = `/projects/${encodeURIComponent(projectId)}/issues`;

  return (
    <>
      <header className="ksq-ticket-detail__head">
        <div className="ksq-ticket-detail__ids">
          <code className="ksq-ticket-id" title={thread.workItemId}>
            {shortId(thread.workItemId)}
          </code>
          <StatusChip state={thread.state} />
          {thread.blockedReason && (
            <span className="ksq-chip" data-tone="blocked" data-testid="detail-blocked">
              blocked
            </span>
          )}
        </div>
        <h1 style={{ margin: "6px 0 0" }}>{thread.title}</h1>
      </header>

      <div className="ksq-ticket-detail__grid">
        <div className="ksq-ticket-detail__main">
          <section className="card" data-testid="detail-description">
            <h2>Description</h2>
            {thread.description ? (
              <p className="ksq-prose">{thread.description}</p>
            ) : (
              <p className="muted">No description.</p>
            )}
          </section>

          <SubTickets state={childrenState} issuesHref={issuesHref} />

          <section className="card" data-testid="detail-activity">
            <h2>Activity</h2>
            {activity.length === 0 ? (
              <p className="muted" data-testid="detail-activity-empty">
                No activity yet.
              </p>
            ) : (
              <ul className="ksq-activity-list">
                {activity.map((item, i) => (
                  <ActivityRow key={`${item.kind}-${i}`} item={item} />
                ))}
              </ul>
            )}

            {/* Honest write gap: the human comment endpoint isn't wired yet. */}
            <div className="ksq-composer-note" data-testid="detail-composer-disabled">
              <textarea
                disabled
                aria-label="Write a message to the agents"
                placeholder="Write a message to the agents…"
              />
              <p className="muted">
                Posting to the agents from the console is coming soon — the
                human comment write path isn’t wired yet.
              </p>
            </div>
          </section>
        </div>

        <aside className="ksq-ticket-detail__side">
          <section className="card" data-testid="detail-properties">
            <h2>Properties</h2>
            <dl className="ksq-proplist">
              <dt className="muted">Status</dt>
              <dd>
                <StatusChip state={thread.state} />
              </dd>
              <dt className="muted">Assignee</dt>
              <dd data-testid="detail-holder">
                {thread.holder ? (
                  <code className="ksq-ticket-id">{thread.holder}</code>
                ) : (
                  <span className="muted">— unclaimed</span>
                )}
              </dd>
              {thread.blockedReason && (
                <>
                  <dt className="muted">Blocked</dt>
                  <dd>{thread.blockedReason}</dd>
                </>
              )}
            </dl>
          </section>

          <section className="card" data-testid="detail-run-trace">
            <h2>Agent run &amp; trace</h2>
            {thread.runId ? (
              <p>
                Run <code className="ksq-ticket-id">{thread.runId}</code>
              </p>
            ) : (
              <p className="muted">No run linked yet.</p>
            )}
            {/* S4: the Dynatrace deep-link renders here once the OBS contract
                (child 897f4d62: OBSERVABILITY_TRACE_URL + work_item.ref format)
                lands. Deferred, not stubbed. */}
            <p className="muted" data-testid="detail-trace-pending">
              End-to-end trace deep-link coming with the observability wiring.
            </p>
          </section>
        </aside>
      </div>
    </>
  );
}

function SubTickets({
  state,
  issuesHref,
}: {
  state: ChildrenState;
  issuesHref: string;
}) {
  return (
    <section className="card" data-testid="detail-subtickets">
      <h2>Sub-tickets</h2>
      {state.kind === "loading" ? (
        <p className="muted" data-testid="detail-subtickets-loading">
          Loading sub-tickets…
        </p>
      ) : state.kind === "not-available" ? (
        <p className="muted">Sub-ticket read model not available here.</p>
      ) : state.kind === "error" ? (
        <p className="muted">
          Sub-tickets unavailable (HTTP {state.status || "network error"}).
        </p>
      ) : state.items.length === 0 ? (
        <p className="muted" data-testid="detail-subtickets-empty">
          No sub-tickets.
        </p>
      ) : (
        <>
          <p className="muted" data-testid="detail-subtickets-progress">
            {subTicketProgress(state.items).done} of{" "}
            {subTicketProgress(state.items).total} done
          </p>
          <ul className="ksq-subticket-list">
            {state.items.map((it) => (
              <li key={it.id} data-testid="detail-subticket">
                <a href={`${issuesHref}/${encodeURIComponent(it.id)}`}>{it.title}</a>
                <span className="ksq-chip ksq-chip--state">{stateLabel(it.state)}</span>
              </li>
            ))}
          </ul>
        </>
      )}
    </section>
  );
}

export default TicketDetail;
