"use client";

// components/tickets/ListView.tsx — the List view (story 8.14c; re-skinned for the
// Paperclip look in ISI-4452 plan §5 S2 + S5/S6).
//
// A read-only, sortable list over the SAME filtered set as the Kanban (8.14d
// shared narrowing). The former <table> is now Paperclip-style row BANDS: each
// row carries a left phase-colour spine, a status chip with a dot, the 8.17
// caret + child-count badge (parents only), id · title · priority · assignee
// (dashed ring = unassigned) · labels · updated. `cancelled` rows are muted +
// struck; a `blocked` row adds a rose warn glyph (§8.6 — blocked is a condition,
// never a lane). Colour comes exclusively from lib/tickets/statusColor (S1), the
// single source of truth shared with the Kanban re-skin.
//
// The list is READ-ONLY: no mutation issues from it (status DnD lives on the
// Kanban card, 8.14b; R6 scope guard). A row's title deep-links to the ticket
// detail (ISI-4399 S3). Sub-ticket parents expand to indented children with a
// connector, keeping the 8.17 lazy load. A 10-status colour legend sits in the
// footer (S5). Optional fields degrade to an honest "—" / dashed ring, never a
// fabricated value (FR-I3).

import { Fragment } from "react";
import { nextSortDir, sortWorkItems } from "@/lib/tickets/derivation";
import { type SortKey, type SortSpec, type WorkItem } from "@/lib/tickets/types";
import { PHASE_STATUSES, statusColor, statusMeta } from "@/lib/tickets/statusColor";
import { INDENT_CAP, TicketTreeToggle, type TreeController } from "./SubTicketTree";

const COLUMNS: ReadonlyArray<{ key: SortKey; label: string }> = [
  { key: "id", label: "ID" },
  { key: "title", label: "Title" },
  { key: "status", label: "Status" },
  { key: "priority", label: "Priority" },
  { key: "assignee", label: "Assignee" },
  { key: "labels", label: "Labels" },
  { key: "updated", label: "Updated" },
];

export interface ListViewProps {
  items: WorkItem[];
  tree: TreeController;
  sort: SortSpec;
  onSortChange: (sort: SortSpec) => void;
  /** Project id for the per-row ticket-detail deep-link (ISI-4399 S3). */
  projectId: string;
}

export function ListView({ items, tree, sort, onSortChange, projectId }: ListViewProps) {
  const detailHref = (id: string) =>
    `/projects/${encodeURIComponent(projectId)}/issues/${encodeURIComponent(id)}`;
  const sorted = sortWorkItems(items, sort);

  function renderRow(item: WorkItem, depth: number) {
    const meta = statusMeta(item.state);
    const beyondCap = depth > INDENT_CAP;
    const indentDepth = Math.min(depth, INDENT_CAP);
    // `cancelled` is an ISI-4455 phase status not yet in the read-model enum;
    // compare as a string so the muted/struck skin is ready the day it lands.
    const cancelled = (item.state as string) === "cancelled";
    const blocked = Boolean(item.blockedReason);
    const rowClass = [
      "ksq-listrow",
      depth > 0 ? "ksq-listrow--child" : "",
      cancelled ? "ksq-listrow--cancelled" : "",
      blocked ? "ksq-listrow--blocked" : "",
    ]
      .filter(Boolean)
      .join(" ");
    return (
      <div
        className={rowClass}
        role="row"
        data-tree-depth={depth}
        data-testid={`row-${item.id}`}
        style={{
          ["--phase-hue" as string]: statusColor(item.state),
          ["--tree-indent-depth" as string]: indentDepth,
        }}
      >
        <span aria-hidden="true" className="ksq-listrow__spine" />

        <span className="ksq-listrow__lead" role="cell">
          {beyondCap && (
            <span
              aria-hidden="true"
              className="ksq-tree-continue"
              title="Continued child (indent capped)"
            >
              ↳
            </span>
          )}
          <span className="ksq-listrow__caret">
            <TicketTreeToggle item={item} tree={tree} />
          </span>
          <span className="ksq-ticket-id" title={item.id}>
            {item.id.slice(0, 8)}
          </span>
        </span>

        <span className="ksq-listrow__title" role="cell">
          {blocked && (
            <span
              className="ksq-warn-glyph"
              data-testid={`blocked-glyph-${item.id}`}
              title={`Blocked: ${item.blockedReason}`}
            >
              ⚠
            </span>
          )}
          <a
            className="ksq-listrow__titletext"
            href={detailHref(item.id)}
            data-testid={`row-title-${item.id}`}
          >
            {item.title}
          </a>
          {item.provenance && (
            <span
              className="ksq-chip ksq-chip--prov"
              data-testid={`prov-${item.id}`}
              title={`Synced from ${item.provenance}`}
            >
              {item.provenance}
            </span>
          )}
        </span>

        <span className="ksq-listrow__status" role="cell">
          <span
            className="ksq-chip ksq-chip--phase"
            data-testid={`row-state-${item.id}`}
            title={meta.role ? `${meta.group} · ${meta.role}` : meta.group}
          >
            <span aria-hidden="true" className="ksq-phase-dot" />
            {meta.label}
          </span>
        </span>

        <span className="ksq-listrow__priority" role="cell">
          {item.priority ? (
            <span className="ksq-chip ksq-chip--priority">{item.priority}</span>
          ) : (
            <span className="ksq-dim">—</span>
          )}
        </span>

        <span className="ksq-listrow__assignee" role="cell">
          {item.assignee ? (
            <>
              <span aria-hidden="true" className="ksq-avatar">
                {initials(item.assignee)}
              </span>
              <span className="ksq-listrow__assignee-name">{item.assignee}</span>
            </>
          ) : (
            <>
              <span aria-hidden="true" className="ksq-avatar ksq-avatar--empty" />
              <span className="ksq-dim">Unassigned</span>
            </>
          )}
        </span>

        <span className="ksq-listrow__labels" role="cell">
          {(item.labels ?? []).map((l) => (
            <span key={l} className="ksq-chip">
              {l}
            </span>
          ))}
          {(item.labels ?? []).length === 0 && <span className="ksq-dim">—</span>}
        </span>

        <span className="ksq-listrow__updated" role="cell">
          {formatUpdated(item.updatedAt)}
        </span>
      </div>
    );
  }

  // Emit the parent band, then — when expanded — its lazily loaded children as
  // sibling bands indented one level (8.17). A child that is itself a parent gets
  // its own caret and recurses. Div bands (not <tr>) let the whole row be a flex
  // band with a coloured spine; the tree is a flat sibling list, no table.
  function renderTreeRows(item: WorkItem, depth: number): React.ReactNode {
    const children = tree.isExpanded(item.id) ? tree.childrenOf(item.id) : undefined;
    return (
      <Fragment key={item.id}>
        {renderRow(item, depth)}
        {children?.map((child) => renderTreeRows(child, depth + 1))}
      </Fragment>
    );
  }

  return (
    <div className="ksq-list" data-testid="tickets-list">
      <div className="ksq-list__head" role="row">
        {COLUMNS.map((col) => {
          const active = sort.key === col.key;
          return (
            <button
              key={col.key}
              type="button"
              className={`ksq-list-sort ksq-list-sort--${col.key}${active ? " is-active" : ""}`}
              data-testid={`sort-${col.key}`}
              aria-label={
                active
                  ? `Sort by ${col.label}, currently ${sort.dir === "asc" ? "ascending" : "descending"}`
                  : `Sort by ${col.label}`
              }
              onClick={() => onSortChange(nextSortDir(sort, col.key))}
            >
              {col.label}
              {active && (
                <span aria-hidden="true" className="ksq-list-sort__dir">
                  {sort.dir === "asc" ? "▲" : "▼"}
                </span>
              )}
            </button>
          );
        })}
      </div>

      <div className="ksq-list__rows">
        {sorted.map((item) => renderTreeRows(item, 0))}
        {sorted.length === 0 && (
          <p className="ksq-empty-hint" data-testid="list-empty">
            No tickets.
          </p>
        )}
      </div>

      <StatusLegend />
    </div>
  );
}

/** The 10-status colour legend (S5) — documents every phase hue + the blocked note. */
function StatusLegend() {
  return (
    <div className="ksq-legend" data-testid="status-legend" aria-label="Status colour legend">
      <span className="ksq-legend__label">Phases</span>
      {PHASE_STATUSES.map((state) => {
        const meta = statusMeta(state);
        return (
          <span
            key={state}
            className="ksq-legend__item"
            data-testid={`legend-${state}`}
            style={{ ["--phase-hue" as string]: statusColor(state) }}
            title={meta.role ? `${meta.group} · ${meta.role}` : meta.group}
          >
            <span aria-hidden="true" className="ksq-phase-dot" />
            {meta.label}
          </span>
        );
      })}
      <span className="ksq-legend__item ksq-legend__item--blocked" data-testid="legend-blocked">
        <span aria-hidden="true" className="ksq-warn-glyph">
          ⚠
        </span>
        Blocked (condition, any phase)
      </span>
    </div>
  );
}

function initials(name: string): string {
  const parts = name.trim().split(/[\s._-]+/).filter(Boolean);
  if (parts.length === 0) return "?";
  if (parts.length === 1) return parts[0].slice(0, 2).toUpperCase();
  return (parts[0][0] + parts[parts.length - 1][0]).toUpperCase();
}

function formatUpdated(iso: string): string {
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? iso : d.toISOString().slice(0, 16).replace("T", " ");
}
