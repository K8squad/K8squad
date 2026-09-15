"use client";

// components/tickets/KanbanBoard.tsx — the Kanban view, re-skinned for the
// ISI-4431 phase lifecycle (ISI-4452 plan §5 S3/S4, gated on ISI-4455's enum).
//
// TEN phase columns — backlog · todo · design · planning · implementation ·
// code_review · testing · documentation · done · cancelled — grouped under the
// four super-headers Intake · Build · Review · Done. Each card is placed by a
// PURE PROJECTION of `work_item.state` (§13 board-derivation; no stored column):
// the two transitional engine lanes (`in_progress` / `in_review`) FOLD onto
// Implementation / Code review (phaseOf), they are never a column of their own.
// A blocked item renders in its phase column with a rose badge overlay — blocked
// is a condition, never a column (§8.6). `cancelled` is a dim, dashed column.
//
// Drag-and-drop is the ONE mutation this screen adds and it is GUARD-AWARE
// (ISI-4457 S4): the client honours a stricter-than-server adjacency graph
// (lib/tickets/transitions) — a disallowed phase jump shows a "no-drop" cue and
// issues NO PATCH (fail-closed; we never preventDefault the dragover so the drop
// event cannot fire). An allowed move issues exactly one
// PATCH /work-items/{id}/state {toState, fromState} (8.14a; apiserver field
// names — ISI-4225) — an audited, RBAC-gated operator override that does NOT
// take the agent's fence claim (§6.2). On 409 the board re-syncs to server truth.
// A viewer (or an unknown role) sees DnD disabled and can issue no PATCH — the UI
// RBAC gate mirroring the §6.7.2 server wall. The quick-move menu is the
// tap/keyboard equivalent of the drag and is guard-limited to allowed targets.
//
// Colour + column order + owning Role come exclusively from lib/tickets/statusColor
// (S1, the shared SSOT) so the Kanban and the List can never drift apart.

import { useMemo, useRef, useState } from "react";
import { canDrag, isBlocked } from "@/lib/tickets/derivation";
import { type WorkItem } from "@/lib/tickets/types";
import {
  PHASE_STATUSES,
  STATUS_META,
  statusColor,
  type PhaseGroup,
  type PhaseStatus,
} from "@/lib/tickets/statusColor";
import { allowedTargets, canTransition, phaseOf } from "@/lib/tickets/transitions";
import { TicketNode, type TreeController } from "./SubTicketTree";

/** The four super-header groups, in lifecycle order (design §1 · Kanban row). */
const GROUP_ORDER: readonly PhaseGroup[] = ["Intake", "Build", "Review", "Done"];

/** Columns per group, DERIVED from the shared SSOT so the two can never drift. */
const GROUPS: ReadonlyArray<{ group: PhaseGroup; phases: readonly PhaseStatus[] }> =
  GROUP_ORDER.map((group) => ({
    group,
    phases: PHASE_STATUSES.filter((s) => STATUS_META[s].group === group),
  }));

export interface KanbanBoardProps {
  /** The visible (already filtered) items — roots of the sub-ticket tree. */
  items: WorkItem[];
  tree: TreeController;
  role: string;
  /** Project id for the per-card ticket-detail deep-link (ISI-4399 S3). */
  projectId: string;
  /** Perform the human status-transition; resolves on 200, throws on 409/error (screen resyncs). */
  onTransition: (item: WorkItem, to: PhaseStatus) => Promise<void>;
}

export function KanbanBoard({ items, tree, role, onTransition, projectId }: KanbanBoardProps) {
  const draggable = canDrag(role);
  const detailHref = (id: string) =>
    `/projects/${encodeURIComponent(projectId)}/issues/${encodeURIComponent(id)}`;
  const dragged = useRef<WorkItem | null>(null);
  const inFlight = useRef<string | null>(null);
  const [draggingId, setDraggingId] = useState<string | null>(null);
  // The column currently under the pointer + whether a drop there is allowed —
  // drives the glow / drop-line (allowed) vs the "no-drop" cue (disallowed).
  const [dropTarget, setDropTarget] = useState<{ phase: PhaseStatus; allowed: boolean } | null>(
    null,
  );
  // Groups the user has collapsed to a slim rail (10 columns exceed one screen).
  const [collapsed, setCollapsed] = useState<ReadonlySet<PhaseGroup>>(new Set());

  // Placement: bucket every visible root by the phase COLUMN its state folds to
  // (§13 pure projection). Unknown states are dropped, never guessed into a lane.
  const byPhase = useMemo(() => {
    const map = new Map<PhaseStatus, WorkItem[]>(PHASE_STATUSES.map((s) => [s, []]));
    for (const it of items) {
      const p = phaseOf(it.state);
      if (p) map.get(p)!.push(it);
    }
    return map;
  }, [items]);

  // How many items each super-group holds (for the collapsed-rail count badge).
  const groupCount = (phases: readonly PhaseStatus[]) =>
    phases.reduce((n, p) => n + (byPhase.get(p)?.length ?? 0), 0);

  // The in-flight engine lanes (folded away as columns) surface as a coordinator
  // hint — honest count, never a fabricated lane.
  const dispatching = items.filter((it) => it.state === "in_progress" || it.state === "in_review")
    .length;

  async function transition(item: WorkItem, to: PhaseStatus) {
    if (!draggable) return; // UI RBAC gate — the server wall (§6.7.2) stays authoritative.
    if (!canTransition(item.state, to)) return; // fail-closed: a disallowed jump issues NO PATCH.
    if (inFlight.current === item.id) return; // exactly ONE PATCH per move (8.14b AC4)
    inFlight.current = item.id;
    try {
      await onTransition(item, to);
    } finally {
      inFlight.current = null;
    }
  }

  function toggleGroup(group: PhaseGroup) {
    setCollapsed((prev) => {
      const next = new Set(prev);
      if (next.has(group)) next.delete(group);
      else next.add(group);
      return next;
    });
  }

  function renderCard(item: WorkItem, toggle: React.ReactNode) {
    const targets = draggable ? allowedTargets(item.state) : [];
    return (
      <article
        key={item.id}
        className={[
          "ksq-kanban-card",
          isBlocked(item) ? "ksq-kanban-card--blocked" : "",
          draggingId === item.id ? "ksq-kanban-card--dragging" : "",
        ]
          .filter(Boolean)
          .join(" ")}
        data-testid={`card-${item.id}`}
        draggable={draggable}
        onDragStart={(evt) => {
          dragged.current = item;
          setDraggingId(item.id);
          if (evt.dataTransfer) {
            evt.dataTransfer.setData("text/plain", item.id);
            evt.dataTransfer.effectAllowed = "move";
          }
        }}
        onDragEnd={() => {
          dragged.current = null;
          setDraggingId(null);
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
        <div className="ksq-kanban-card__title">
          <a href={detailHref(item.id)} data-testid={`card-title-${item.id}`}>
            {item.title}
          </a>
        </div>
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
            <span aria-hidden="true" className="ksq-quickmove__arrow">
              →
            </span>
            <select
              data-testid={`quick-move-${item.id}`}
              value=""
              disabled={targets.length === 0}
              onChange={(evt) => {
                const to = evt.target.value as PhaseStatus;
                if (to) void transition(item, to);
              }}
            >
              <option value="" disabled>
                Move to…
              </option>
              {targets.map((s) => (
                <option key={s} value={s}>
                  {STATUS_META[s].label}
                </option>
              ))}
            </select>
          </label>
        )}
      </article>
    );
  }

  function renderColumn(phase: PhaseStatus) {
    const meta = STATUS_META[phase];
    const laneItems = byPhase.get(phase) ?? [];
    const quiet = laneItems.length === 0;
    const isOver = dropTarget?.phase === phase;
    const overAllowed = isOver && dropTarget.allowed;
    const overBlocked = isOver && !dropTarget.allowed;
    return (
      <section
        key={phase}
        className={[
          "ksq-kanban-column",
          `ksq-kanban-column--${phase}`,
          quiet ? "ksq-kanban-column--quiet" : "",
          phase === "cancelled" ? "ksq-kanban-column--cancelled" : "",
          overAllowed ? "ksq-kanban-column--over" : "",
          overBlocked ? "ksq-kanban-column--nodrop" : "",
        ]
          .filter(Boolean)
          .join(" ")}
        data-testid={`column-${phase}`}
        aria-label={meta.label}
        style={{ ["--phase-hue" as string]: statusColor(phase) }}
        onDragOver={(evt) => {
          const item = dragged.current;
          if (!draggable || item == null) return;
          const allowed = canTransition(item.state, phase);
          // ONLY an allowed target opts into the drop (preventDefault). A
          // disallowed column shows the no-drop cue but never fires `drop` — the
          // fail-closed guarantee that a bad jump issues no PATCH.
          if (allowed) {
            evt.preventDefault();
            if (evt.dataTransfer) evt.dataTransfer.dropEffect = "move";
          } else if (evt.dataTransfer) {
            evt.dataTransfer.dropEffect = "none";
          }
          setDropTarget({ phase, allowed });
        }}
        onDragLeave={() => setDropTarget((t) => (t && t.phase === phase ? null : t))}
        onDrop={(evt) => {
          evt.preventDefault();
          const item = dragged.current;
          dragged.current = null;
          setDraggingId(null);
          setDropTarget(null);
          if (item) void transition(item, phase); // transition() re-checks the guard
        }}
      >
        <header className="ksq-kanban-column__head">
          <span aria-hidden="true" className="ksq-kanban-column__bar" />
          <span aria-hidden="true" className="ksq-phase-dot" />
          <h3 className="ksq-kanban-column__name">{meta.label}</h3>
          <span
            className="ksq-kanban-column__count"
            data-testid={`column-count-${phase}`}
          >
            {laneItems.length}
          </span>
          {overAllowed && (
            <span className="ksq-kanban-column__cue" data-testid={`drop-cue-${phase}`}>
              → {meta.label}
            </span>
          )}
          {draggable && (
            <button
              type="button"
              className="ksq-kanban-column__add"
              data-testid={`column-add-${phase}`}
              aria-label={`New ticket in ${meta.label}`}
              title={`New ticket in ${meta.label}`}
              disabled
            >
              +
            </button>
          )}
        </header>
        {meta.role && (
          <div className="ksq-kanban-column__owner" data-testid={`column-owner-${phase}`}>
            <span aria-hidden="true" className="ksq-kanban-owner-dot" />
            {meta.role}
          </div>
        )}
        <div className="ksq-kanban-column__cards">
          {overAllowed && (
            <div
              aria-hidden="true"
              className="ksq-kanban-dropline"
              data-testid={`dropline-${phase}`}
            />
          )}
          {laneItems.map((item) => (
            <TicketNode key={item.id} item={item} depth={0} tree={tree} renderNode={renderCard} />
          ))}
          {quiet && <p className="ksq-empty-hint">No tickets.</p>}
        </div>
      </section>
    );
  }

  return (
    <div className="ksq-kanban-wrap" data-testid="kanban-wrap">
      <div className="ksq-coordinator-strip" data-testid="coordinator-strip">
        <span className="ksq-coordinator-strip__label">Coordinator</span>
        <span className="ksq-coordinator-strip__note">
          Dispatches work across the phase lifecycle — column moves here are audited
          human overrides, not agent claims.
        </span>
        <span className="ksq-coordinator-strip__count" data-testid="coordinator-inflight">
          {dispatching} in flight
        </span>
      </div>

      <div className="ksq-kanban" data-testid="kanban-board">
        {GROUPS.map(({ group, phases }) => {
          const isCollapsed = collapsed.has(group);
          return (
            <section
              key={group}
              className={`ksq-phase-group${isCollapsed ? " ksq-phase-group--collapsed" : ""}`}
              data-testid={`phase-group-${group}`}
              aria-label={group}
            >
              <header className="ksq-phase-group__head">
                <button
                  type="button"
                  className="ksq-phase-group__toggle"
                  data-testid={`group-toggle-${group}`}
                  aria-expanded={!isCollapsed}
                  onClick={() => toggleGroup(group)}
                >
                  <span aria-hidden="true" className="ksq-phase-group__chevron">
                    {isCollapsed ? "▸" : "▾"}
                  </span>
                  <span className="ksq-phase-group__name">{group}</span>
                  <span className="ksq-phase-group__count">{groupCount(phases)}</span>
                </button>
              </header>
              {!isCollapsed && (
                <div className="ksq-phase-group__cols">{phases.map(renderColumn)}</div>
              )}
            </section>
          );
        })}
      </div>
    </div>
  );
}
