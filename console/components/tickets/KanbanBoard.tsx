"use client";

// components/tickets/KanbanBoard.tsx — the Kanban view (story 8.14b).
//
// Five columns — Backlog · Todo · In Progress · In Review · Done — each item
// placed by `work_item.state` as a PURE PROJECTION (§13 board-derivation; no
// stored `column` is consulted anywhere). A blocked item renders in its lane
// with a badge overlay — blocked is a condition, never a 6th column (§8.6).
//
// Drag-and-drop is the ONE mutation this screen adds: dropping a card issues
// exactly one PATCH /work-items/{id}/state {to, expectedFrom} (8.14a) — an
// audited, RBAC-gated operator override that does NOT take the agent's fence
// claim (§6.2 — no claim/lease call ever fires from here). On 409 the board
// re-syncs to server truth (the screen refetches; no client-authored state).
// A viewer (or when the role is unknown) sees DnD disabled and can issue no
// PATCH — the UI RBAC gate mirroring the §6.7.2 server wall. A quick-move
// control provides the tap/keyboard equivalent of the drag (responsive spec,
// Tickets — Kanban row).

import { useRef, useState } from "react";
import {
  canDrag,
  deriveColumns,
  isBlocked,
} from "@/lib/tickets/derivation";
import {
  STATE_LABELS,
  WORK_ITEM_STATES,
  type WorkItem,
  type WorkItemState,
} from "@/lib/tickets/types";
import { TicketNode, type TreeController } from "./SubTicketTree";

export interface KanbanBoardProps {
  /** The visible (already filtered) items — roots of the sub-ticket tree. */
  items: WorkItem[];
  tree: TreeController;
  role: string;
  /** Perform the human status-transition; resolves on 200, throws on 409/error (screen resyncs). */
  onTransition: (item: WorkItem, to: WorkItemState) => Promise<void>;
}

export function KanbanBoard({ items, tree, role, onTransition }: KanbanBoardProps) {
  const draggable = canDrag(role);
  const dragged = useRef<WorkItem | null>(null);
  const inFlight = useRef<string | null>(null);
  const [dropTarget, setDropTarget] = useState<WorkItemState | null>(null);
  const columns = deriveColumns(items);

  async function transition(item: WorkItem, to: WorkItemState) {
    if (!draggable) return; // UI RBAC gate — the server wall (§6.7.2) stays authoritative.
    if (item.state === to) return;
    if (inFlight.current === item.id) return; // exactly ONE PATCH per move (8.14b AC4)
    inFlight.current = item.id;
    try {
      await onTransition(item, to);
    } finally {
      inFlight.current = null;
    }
  }

  function renderCard(item: WorkItem, toggle: React.ReactNode) {
    return (
      <article
        key={item.id}
        className={`ksq-kanban-card${isBlocked(item) ? " ksq-kanban-card--blocked" : ""}`}
        data-testid={`card-${item.id}`}
        draggable={draggable}
        onDragStart={(evt) => {
          dragged.current = item;
          evt.dataTransfer.setData("text/plain", item.id);
          evt.dataTransfer.effectAllowed = "move";
        }}
        onDragEnd={() => {
          dragged.current = null;
          setDropTarget(null);
        }}
      >
        {isBlocked(item) && (
          <span className="ksq-kanban-blocked" data-testid={`blocked-badge-${item.id}`}>
            Blocked
          </span>
        )}
        <div className="ksq-kanban-card__head">
          <span className="ksq-ticket-id" title={item.id}>
            {item.id.slice(0, 8)}
          </span>
          {toggle}
        </div>
        <div className="ksq-kanban-card__title">{item.title}</div>
        <div className="ksq-kanban-card__meta">
          <span className="ksq-chip" data-testid={`card-priority-${item.id}`}>
            {item.priority ?? "—"}
          </span>
          <span className="ksq-chip" data-testid={`card-assignee-${item.id}`}>
            {item.assignee ?? "unassigned"}
          </span>
        </div>
        {draggable && (
          <label className="ksq-quickmove">
            <span className="ksq-sr-only">Move {item.title} to</span>
            <select
              data-testid={`quick-move-${item.id}`}
              value={item.state}
              onChange={(evt) => void transition(item, evt.target.value as WorkItemState)}
            >
              {WORK_ITEM_STATES.map((s) => (
                <option key={s} value={s}>
                  {STATE_LABELS[s]}
                </option>
              ))}
            </select>
          </label>
        )}
      </article>
    );
  }

  return (
    <div className="ksq-kanban" data-testid="kanban-board">
      {columns.map((col) => (
        <section
          key={col.state}
          className={`ksq-kanban-column${dropTarget === col.state ? " ksq-kanban-column--over" : ""}`}
          data-testid={`column-${col.state}`}
          aria-label={STATE_LABELS[col.state]}
          onDragOver={(evt) => {
            if (!draggable || dragged.current == null) return;
            evt.preventDefault();
            evt.dataTransfer.dropEffect = "move";
            setDropTarget(col.state);
          }}
          onDragLeave={() => setDropTarget((t) => (t === col.state ? null : t))}
          onDrop={(evt) => {
            evt.preventDefault();
            setDropTarget(null);
            const item = dragged.current;
            dragged.current = null;
            if (item) void transition(item, col.state);
          }}
        >
          <header className="ksq-kanban-column__head">
            <h3>{STATE_LABELS[col.state]}</h3>
            <span className="ksq-kanban-column__count" data-testid={`column-count-${col.state}`}>
              {col.items.length}
            </span>
          </header>
          <div className="ksq-kanban-column__cards">
            {col.items.map((item) => (
              <TicketNode
                key={item.id}
                item={item}
                depth={0}
                tree={tree}
                renderNode={renderCard}
              />
            ))}
            {col.items.length === 0 && (
              <p className="ksq-empty-hint">No tickets.</p>
            )}
          </div>
        </section>
      ))}
    </div>
  );
}
