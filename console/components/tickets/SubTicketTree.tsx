"use client";

// components/tickets/SubTicketTree.tsx — the reusable sub-ticket tree (story 8.17,
// ADR-036). Shared by the Kanban card (8.14b) and the List row (8.14c) so the
// parent/child hierarchy reads the same everywhere.
//
// Contract (8.17 ACs):
//   • disclosure caret + child-count badge on parents; a leaf shows NO caret;
//   • expanding LAZY-LOADS that parent's direct children (BFF GET …?parentId=),
//     rendered indented one level with status/priority/assignee chips;
//   • expansion state is CLIENT-ONLY view state (localStorage/URL) — no mutation;
//   • deep nesting recurses; beyond an indent cap a "continue ↳" marker replaces
//     further indentation (no unbounded indent);
//   • orphans are roots by construction (parent_id ON DELETE SET NULL, §6.1) —
//     the tree only ever renders items the read model returned;
//   • READ + NAVIGATE ONLY — no drag/claim/mutate on tree nodes (R6 scope guard;
//     status DnD lives on the Kanban card per 8.14);
//   • keyboard-accessible: the caret is a real <button aria-expanded>; arrow-key
//     tree navigation moves focus between carets.

import { useCallback, type ReactNode } from "react";
import type { WorkItem } from "@/lib/tickets/types";

/** Indent depth cap — beyond this a "continue ↳" marker replaces deeper indentation. */
export const INDENT_CAP = 4;

/** The tree's client-only controller, owned by the screen (childrenCache + expanded set). */
export interface TreeController {
  /** Direct children loaded for a parent (undefined ⇒ not yet loaded). */
  childrenOf(parentId: string): WorkItem[] | undefined;
  /** Known child count (server-provided childCount, else cached children length, else 0). */
  childCountOf(item: WorkItem): number;
  isExpanded(parentId: string): boolean;
  toggle(parentId: string): void;
}

export function childrenDomId(itemId: string): string {
  return `ksq-tree-children-${itemId}`;
}

/**
 * The disclosure control: caret + child-count badge. A leaf (count ≤ 0) renders
 * nothing — 8.17 AC1. ≥44px hit area, real button, aria-expanded/aria-controls.
 */
export function TicketTreeToggle({
  item,
  tree,
}: {
  item: WorkItem;
  tree: TreeController;
}) {
  const count = tree.childCountOf(item);
  if (count <= 0) return null;
  const expanded = tree.isExpanded(item.id);
  return (
    <button
      type="button"
      className="ksq-tree-caret"
      data-tree-caret=""
      data-item-id={item.id}
      data-testid={`tree-caret-${item.id}`}
      aria-expanded={expanded}
      aria-controls={childrenDomId(item.id)}
      aria-label={`${expanded ? "Collapse" : "Expand"} sub-tickets (${count})`}
      onClick={() => tree.toggle(item.id)}
    >
      <span aria-hidden="true" className="ksq-tree-caret__arrow">
        {expanded ? "▾" : "▸"}
      </span>
      <span className="ksq-tree-badge" data-testid={`child-count-${item.id}`}>
        {count}
      </span>
    </button>
  );
}

/**
 * One tree node: caller-supplied content (Kanban card / List row) + its toggle,
 * followed by the lazily-loaded, indented children when expanded. Recursive —
 * a child that is itself a parent gets its own caret/badge (8.17 AC3).
 */
export function TicketNode({
  item,
  depth,
  tree,
  renderNode,
}: {
  item: WorkItem;
  depth: number;
  tree: TreeController;
  renderNode: (item: WorkItem, toggle: ReactNode, depth: number) => ReactNode;
}) {
  const beyondCap = depth > INDENT_CAP;
  const indentDepth = Math.min(depth, INDENT_CAP);
  return (
    <div
      className={`ksq-tree-node${beyondCap ? " ksq-tree-node--capped" : ""}`}
      data-tree-depth={depth}
      style={{ ["--tree-indent-depth" as string]: indentDepth }}
    >
      {beyondCap && (
        <span aria-hidden="true" className="ksq-tree-continue" title="Continued child (indent capped)">
          ↳
        </span>
      )}
      {renderNode(item, <TicketTreeToggle item={item} tree={tree} />, depth)}
      {tree.isExpanded(item.id) && (
        <SubTicketChildren
          parent={item}
          depth={depth}
          tree={tree}
          renderNode={renderNode}
        />
      )}
    </div>
  );
}

/** The expanded, indented children of a parent (8.17 AC2 — lazy, one level per hop). */
export function SubTicketChildren({
  parent,
  depth,
  tree,
  renderNode,
}: {
  parent: WorkItem;
  depth: number;
  tree: TreeController;
  renderNode: (item: WorkItem, toggle: ReactNode, depth: number) => ReactNode;
}) {
  const children = tree.childrenOf(parent.id) ?? [];
  return (
    <ul
      className="ksq-tree-children"
      id={childrenDomId(parent.id)}
      role="group"
      aria-label={`Sub-tickets of ${parent.title}`}
      data-testid={`tree-children-${parent.id}`}
    >
      {children.map((child) => (
        <li key={child.id} role="none">
          <TicketNode
            item={child}
            depth={depth + 1}
            tree={tree}
            renderNode={renderNode}
          />
        </li>
      ))}
      {children.length === 0 && (
        <li className="ksq-tree-empty" role="none">
          No sub-tickets.
        </li>
      )}
    </ul>
  );
}

/**
 * Arrow-key tree navigation (8.17 AC6): ↓/↑ move focus between the visible
 * caret buttons in DOM order, → expands the focused caret, ← collapses it.
 * Attach to the tree container via onKeyDown.
 */
export function useTreeKeyboardNav() {
  return useCallback((evt: React.KeyboardEvent<HTMLDivElement>) => {
    const carets = Array.from(
      evt.currentTarget.querySelectorAll<HTMLButtonElement>("[data-tree-caret]"),
    );
    if (carets.length === 0) return;
    const current = carets.indexOf(document.activeElement as HTMLButtonElement);
    switch (evt.key) {
      case "ArrowDown": {
        const next = carets[(current + 1 + carets.length) % carets.length];
        next.focus();
        evt.preventDefault();
        break;
      }
      case "ArrowUp": {
        const prev = carets[(current - 1 + carets.length) % carets.length];
        prev.focus();
        evt.preventDefault();
        break;
      }
      case "ArrowRight":
      case "ArrowLeft": {
        if (current >= 0) {
          const wantExpanded = evt.key === "ArrowRight";
          const isExpanded = carets[current].getAttribute("aria-expanded") === "true";
          if (isExpanded !== wantExpanded) carets[current].click();
          evt.preventDefault();
        }
        break;
      }
      default:
        break;
    }
  }, []);
}
