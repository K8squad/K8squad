"use client";

// components/tickets/ListView.tsx — the List view (story 8.14c).
//
// A sortable table — ID · Title · Status · Priority · Assignee · Labels · Updated —
// over the SAME filtered set as the Kanban (8.14d shared narrowing). The table is
// READ-ONLY: no mutation issues from it (status DnD lives on the Kanban card,
// 8.14b; R6 scope guard). Parent rows carry the 8.17 caret + child-count badge
// and expand to indented children. SCM-synced items keep their provenance badge
// (§5.4). Responsive (spec, Tickets — List row): under 720px the table shows
// ID + Title + Status and a row-expand reveals the remaining fields; sort stays
// server-side-identical regardless of viewport.

import { Fragment, useState } from "react";
import {
  nextSortDir,
  sortWorkItems,
} from "@/lib/tickets/derivation";
import {
  STATE_LABELS,
  type SortKey,
  type SortSpec,
  type WorkItem,
} from "@/lib/tickets/types";
import { INDENT_CAP, TicketTreeToggle, type TreeController } from "./SubTicketTree";

const COLUMNS: ReadonlyArray<{ key: SortKey; label: string; className: string }> = [
  { key: "id", label: "ID", className: "ksq-col-id" },
  { key: "title", label: "Title", className: "ksq-col-title" },
  { key: "status", label: "Status", className: "ksq-col-status" },
  { key: "priority", label: "Priority", className: "ksq-col-priority" },
  { key: "assignee", label: "Assignee", className: "ksq-col-assignee" },
  { key: "labels", label: "Labels", className: "ksq-col-labels" },
  { key: "updated", label: "Updated", className: "ksq-col-updated" },
];

export interface ListViewProps {
  items: WorkItem[];
  tree: TreeController;
  sort: SortSpec;
  onSortChange: (sort: SortSpec) => void;
}

export function ListView({ items, tree, sort, onSortChange }: ListViewProps) {
  const [expandedRows, setExpandedRows] = useState<ReadonlySet<string>>(new Set());
  const sorted = sortWorkItems(items, sort);

  function renderRow(item: WorkItem, depth: number) {
    const mobileOpen = expandedRows.has(item.id);
    const beyondCap = depth > INDENT_CAP;
    const indentDepth = Math.min(depth, INDENT_CAP);
    return (
      <Fragment key={item.id}>
        <tr
          className={`ksq-list-row${depth > 0 ? " ksq-list-row--child" : ""}`}
          data-tree-depth={depth}
          data-testid={`row-${item.id}`}
        >
          <td className="ksq-col-id">
            <button
              type="button"
              className="ksq-rowexpand"
              aria-expanded={mobileOpen}
              aria-label={`More fields for ${item.title}`}
              data-testid={`row-expand-${item.id}`}
              onClick={() =>
                setExpandedRows((prev) => {
                  const next = new Set(prev);
                  if (next.has(item.id)) next.delete(item.id);
                  else next.add(item.id);
                  return next;
                })
              }
            >
              <span aria-hidden="true">{mobileOpen ? "▾" : "▸"}</span>
            </button>
            <span className="ksq-ticket-id" title={item.id}>
              {item.id.slice(0, 8)}
            </span>
          </td>
          <td className="ksq-col-title">
            <span
              className="ksq-list-title"
              style={{ ["--tree-indent-depth" as string]: indentDepth }}
            >
              {beyondCap && (
                <span
                  aria-hidden="true"
                  className="ksq-tree-continue"
                  title="Continued child (indent capped)"
                >
                  ↳
                </span>
              )}
              <TicketTreeToggle item={item} tree={tree} />
              <span className="ksq-list-title__text">{item.title}</span>
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
          </td>
          <td className="ksq-col-status">
            <span className="ksq-chip ksq-chip--state" data-testid={`row-state-${item.id}`}>
              {STATE_LABELS[item.state]}
            </span>
          </td>
          <td className="ksq-col-priority">{item.priority ?? "—"}</td>
          <td className="ksq-col-assignee">{item.assignee ?? "unassigned"}</td>
          <td className="ksq-col-labels">
            {(item.labels ?? []).map((l) => (
              <span key={l} className="ksq-chip">
                {l}
              </span>
            ))}
            {(item.labels ?? []).length === 0 && "—"}
          </td>
          <td className="ksq-col-updated">{formatUpdated(item.updatedAt)}</td>
        </tr>
        {mobileOpen && (
          <tr className="ksq-list-rowdetail" data-testid={`row-detail-${item.id}`}>
            <td colSpan={COLUMNS.length}>
              <dl className="ksq-list-detail">
                <div>
                  <dt>Priority</dt>
                  <dd>{item.priority ?? "—"}</dd>
                </div>
                <div>
                  <dt>Assignee</dt>
                  <dd>{item.assignee ?? "unassigned"}</dd>
                </div>
                <div>
                  <dt>Labels</dt>
                  <dd>{(item.labels ?? []).join(", ") || "—"}</dd>
                </div>
                <div>
                  <dt>Updated</dt>
                  <dd>{formatUpdated(item.updatedAt)}</dd>
                </div>
              </dl>
            </td>
          </tr>
        )}
      </Fragment>
    );
  }

  // Table-correct tree: emit the parent row then, when expanded, its lazily
  // loaded children as sibling <tr> rows indented one level (8.17). Children
  // that are themselves parents get their own caret and recurse. Rendering the
  // tree as flat <tr> siblings (not nested <div>/<ul>) keeps the DOM valid
  // inside <tbody> — a <div> wrapper would be hoisted out and break the table.
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
    <div className="ksq-list-scroll" data-testid="tickets-list">
      <table className="ksq-list-table">
        <thead>
          <tr>
            {COLUMNS.map((col) => {
              const active = sort.key === col.key;
              const dir = active ? sort.dir : undefined;
              return (
                <th key={col.key} className={col.className} scope="col" aria-sort={ariaSort(dir)}>
                  <button
                    type="button"
                    className="ksq-list-sort"
                    data-testid={`sort-${col.key}`}
                    onClick={() => onSortChange(nextSortDir(sort, col.key))}
                  >
                    {col.label}
                    {active && (
                      <span aria-hidden="true" className="ksq-list-sort__dir">
                        {sort.dir === "asc" ? "▲" : "▼"}
                      </span>
                    )}
                  </button>
                </th>
              );
            })}
          </tr>
        </thead>
        <tbody>
          {sorted.map((item) => renderTreeRows(item, 0))}
          {sorted.length === 0 && (
            <tr>
              <td colSpan={COLUMNS.length} className="ksq-empty-hint">
                No tickets.
              </td>
            </tr>
          )}
        </tbody>
      </table>
    </div>
  );
}

function ariaSort(dir: "asc" | "desc" | undefined): "ascending" | "descending" | "none" {
  if (dir === "asc") return "ascending";
  if (dir === "desc") return "descending";
  return "none";
}

function formatUpdated(iso: string): string {
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? iso : d.toISOString().slice(0, 16).replace("T", " ");
}
