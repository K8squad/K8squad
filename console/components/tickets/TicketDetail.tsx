"use client";

// components/tickets/TicketDetail.tsx — the ticket-detail screen.
//
// ISI-4447 (S1 of the ISI-4433 redesign) re-lays-out the original flat ISI-4399
// screen into the board's prescribed shape: a two-column page with the
// **Properties rail pinned right**, a Header+Description card at the top of the
// main column, and a dedicated **Sub-tickets status** card (progress bar + done /
// in-progress / todo tiles + Add sub-ticket) in the rail. Frame 01 of
// NAS ksquad/docs/bmad/ux/isi-4433-ticket-detail-redesign/ is the layout contract.
//
// Everything here is still a REPROJECTION of the two reads the console already
// owns (no new backend) — the redesign moves pieces, it does not invent data:
//   • GET /api/work-items/{id}  → the thread (comments + status history + change
//     refs + holder/run), via fetchWorkItemThread.
//   • GET /api/projects/{id}/work-items?parentId={id} → the sub-ticket rows, for
//     both the rail status roll-up and the main-column list.
// Each read is its own async boundary so a slow/absent children read never blanks
// the ticket body (per-panel isolation).
//
// HONEST FIELDS (FR-I3): the M1.5 read models expose title / description / state /
// blocked-reason / holder / run for a ticket — they do NOT carry priority,
// work-mode, parent or labels yet. Those Properties rows render an explicit "—"
// rather than a fabricated value; they light up automatically when the read model
// grows the columns. The dependency-tree (S2), agent-run→comment renderer (S3) and
// sticky prompt-to-agent composer (S4) mount into the marked regions of the main
// column as their child tickets land; S1 ships their honest interim surfaces.

import { useCallback, useEffect, useState } from "react";
import { ApiError, listWorkItems } from "@/lib/tickets/api";
import {
  buildActivity,
  fetchWorkItemThread,
  subTicketProgress,
  subTicketStatus,
  type ActivityItem,
  type NormalizedThread,
} from "@/lib/tickets/thread";
import { STATE_LABELS, type WorkItem, type WorkItemState } from "@/lib/tickets/types";
import { CreateTicketSheet } from "./CreateTicketSheet";

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

function useChildren(
  projectId: string,
  parentId: string,
  reloadKey: number,
): ChildrenState {
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
  }, [projectId, parentId, reloadKey]);
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
  // Children reload after an inline "Add sub-ticket" create so the rail roll-up
  // and the main list re-sync to server truth (no optimistic fabrication).
  const [childReload, setChildReload] = useState(0);
  const children = useChildren(projectId, workItemId, childReload);
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
          onChildCreated={() => setChildReload((k) => k + 1)}
        />
      )}
    </div>
  );
}

function TicketBody({
  projectId,
  thread,
  childrenState,
  onChildCreated,
}: {
  projectId: string;
  thread: NormalizedThread;
  childrenState: ChildrenState;
  onChildCreated: () => void;
}) {
  const activity = buildActivity(thread);
  const issuesHref = `/projects/${encodeURIComponent(projectId)}/issues`;
  const [addingSub, setAddingSub] = useState(false);

  // The one WorkItem the "Add sub-ticket" sheet offers as a parent candidate:
  // this very ticket, synthesized from the thread we already loaded.
  const selfAsParent: WorkItem = {
    id: thread.workItemId,
    projectId,
    parentId: null,
    title: thread.title,
    state: (thread.state as WorkItemState) ?? "backlog",
    blockedReason: thread.blockedReason || null,
    updatedAt: new Date(0).toISOString(),
  };

  return (
    <div className="ksq-ticket-detail__grid">
      {/* ---- Main column: header+description, then S2/S3/S4 mount regions ---- */}
      <div className="ksq-ticket-detail__main">
        <section
          className="card ksq-ticket-detail__header"
          data-testid="detail-header"
        >
          <div className="ksq-ticket-detail__ids">
            <code className="ksq-ticket-id" title={thread.workItemId}>
              {shortId(thread.workItemId)}
            </code>
            <StatusChip state={thread.state} />
            {thread.blockedReason && (
              <span
                className="ksq-chip"
                data-tone="blocked"
                data-testid="detail-blocked"
              >
                blocked
              </span>
            )}
          </div>
          <h1 className="ksq-ticket-detail__title">{thread.title}</h1>
          {/* Opener meta: the read model carries no created-at/opener for the item
              itself, so we surface the honest thing we do have — the current
              holder and the most recent status move (if any). Never fabricated. */}
          <p className="ksq-ticket-detail__meta muted" data-testid="detail-meta">
            {thread.holder ? (
              <>
                Held by{" "}
                <code className="ksq-ticket-id">{thread.holder}</code>
              </>
            ) : (
              <>Unclaimed</>
            )}
            {thread.statusHistory.length > 0 && (
              <>
                {" · updated "}
                <time className="ksq-ticket-id">
                  {fmt(
                    thread.statusHistory[thread.statusHistory.length - 1]
                      .occurredAt,
                  )}
                </time>
              </>
            )}
          </p>
          <hr className="ksq-hairline" />
          <div data-testid="detail-description">
            {thread.description ? (
              <p className="ksq-prose">{thread.description}</p>
            ) : (
              <p className="muted">No description.</p>
            )}
          </div>
        </section>

        {/* S2 mount region — the dependency-tree component (ISI-4448) replaces this
            flat list with a parent → this-ticket → sub-tickets tree. Until then the
            honest sub-ticket list stands in. */}
        <SubTickets state={childrenState} issuesHref={issuesHref} />

        {/* S3 mount region — the agent-run → GitHub-style comment renderer
            (ISI-4449). The chronological Activity thread is the interim surface. */}
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

          {/* S4 mount region — the sticky prompt-to-agent composer (ISI-4450). The
              human comment write path isn't wired yet, so it stays honestly
              disabled rather than posting to an endpoint that isn't there. */}
          <div className="ksq-composer-note" data-testid="detail-composer-disabled">
            <textarea
              disabled
              aria-label="Write a message to the agents"
              placeholder="Write a message to the agents…"
            />
            <p className="muted">
              Posting to the agents from the console is coming soon — the human
              comment write path isn’t wired yet.
            </p>
          </div>
        </section>
      </div>

      {/* ---- Right rail: pinned Properties + Sub-tickets status + Run & trace ---- */}
      <aside className="ksq-ticket-detail__side">
        <section className="card" data-testid="detail-properties">
          <h2>Properties</h2>
          <dl className="ksq-proplist">
            <dt className="muted">Project</dt>
            <dd data-testid="prop-project">
              <code className="ksq-ticket-id">{projectId}</code>
            </dd>

            <dt className="muted">Status</dt>
            <dd>
              <StatusChip state={thread.state} />
            </dd>

            <dt className="muted">Assignee</dt>
            <dd data-testid="detail-holder">
              {thread.holder ? (
                <code className="ksq-ticket-id">{thread.holder}</code>
              ) : (
                <span className="muted">unassigned</span>
              )}
            </dd>

            {/* Priority / Work mode / Parent / Labels are not carried by the M1.5
                read model yet — render an honest em-dash, not a guess (FR-I3). */}
            <dt className="muted">Priority</dt>
            <dd data-testid="prop-priority">
              <span className="muted">—</span>
            </dd>

            <dt className="muted">Work mode</dt>
            <dd data-testid="prop-workmode">
              <span className="muted">—</span>
            </dd>

            <dt className="muted">Parent</dt>
            <dd data-testid="prop-parent">
              <span className="muted">—</span>
            </dd>

            <dt className="muted">Labels</dt>
            <dd data-testid="prop-labels">
              <span className="muted">—</span>
            </dd>

            {thread.blockedReason && (
              <>
                <dt className="muted">Blocked</dt>
                <dd>{thread.blockedReason}</dd>
              </>
            )}
          </dl>
        </section>

        <SubTicketStatusCard
          state={childrenState}
          onAdd={() => setAddingSub(true)}
        />

        <section className="card" data-testid="detail-run-trace">
          <h2>Agent run &amp; trace</h2>
          {thread.runId ? (
            <p>
              Run <code className="ksq-ticket-id">{thread.runId}</code>
            </p>
          ) : (
            <p className="muted">No run linked yet.</p>
          )}
          {/* The Dynatrace deep-link renders here once the OBS contract
              (OBSERVABILITY_TRACE_URL + ksquad.work_item.ref) lands. Deferred. */}
          <p className="muted" data-testid="detail-trace-pending">
            End-to-end trace deep-link coming with the observability wiring.
          </p>
        </section>
      </aside>

      {addingSub && (
        <CreateTicketSheet
          projectId={projectId}
          parents={[selfAsParent]}
          onCreated={() => {
            onChildCreated();
          }}
          onClose={() => setAddingSub(false)}
        />
      )}
    </div>
  );
}

/**
 * Right-rail "Sub-tickets status" card (ISI-4447 S1): the board's "status of the
 * sub tickets" — a proportional progress bar + three count tiles (done /
 * in-progress / todo) + an Add sub-ticket affordance. Honest empty/absent states
 * mirror the main list (never a fabricated roll-up).
 */
function SubTicketStatusCard({
  state,
  onAdd,
}: {
  state: ChildrenState;
  onAdd: () => void;
}) {
  return (
    <section className="card" data-testid="detail-subticket-status">
      <h2>Sub-tickets status</h2>
      {state.kind === "loading" ? (
        <p className="muted">Loading…</p>
      ) : state.kind === "not-available" ? (
        <p className="muted">Sub-ticket read model not available here.</p>
      ) : state.kind === "error" ? (
        <p className="muted">
          Unavailable (HTTP {state.status || "network error"}).
        </p>
      ) : (
        <StatusRollup items={state.items} />
      )}
      <button
        type="button"
        className="ksq-btn ksq-subticket-add"
        data-testid="detail-add-subticket"
        onClick={onAdd}
      >
        + Add sub-ticket
      </button>
    </section>
  );
}

function StatusRollup({ items }: { items: WorkItem[] }) {
  const s = subTicketStatus(items);
  const pct = (n: number) => (s.total === 0 ? 0 : (n / s.total) * 100);
  return (
    <>
      <p className="muted" data-testid="detail-subticket-summary">
        {s.done} of {s.total} done
      </p>
      <div
        className="ksq-progress"
        role="progressbar"
        aria-valuenow={s.done}
        aria-valuemin={0}
        aria-valuemax={s.total}
        aria-label="Sub-tickets done"
        data-testid="detail-progressbar"
      >
        {s.total > 0 && (
          <>
            <span
              className="ksq-progress__seg ksq-progress__seg--done"
              style={{ width: `${pct(s.done)}%` }}
            />
            <span
              className="ksq-progress__seg ksq-progress__seg--inprogress"
              style={{ width: `${pct(s.inProgress)}%` }}
            />
            <span
              className="ksq-progress__seg ksq-progress__seg--todo"
              style={{ width: `${pct(s.todo)}%` }}
            />
          </>
        )}
      </div>
      <ul className="ksq-count-tiles">
        <li className="ksq-count-tile" data-testid="detail-count-done">
          <span className="ksq-count-tile__n">{s.done}</span>
          <span className="ksq-count-tile__label muted">Done</span>
        </li>
        <li className="ksq-count-tile" data-testid="detail-count-inprogress">
          <span className="ksq-count-tile__n">{s.inProgress}</span>
          <span className="ksq-count-tile__label muted">In progress</span>
        </li>
        <li className="ksq-count-tile" data-testid="detail-count-todo">
          <span className="ksq-count-tile__n">{s.todo}</span>
          <span className="ksq-count-tile__label muted">Todo</span>
        </li>
      </ul>
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
