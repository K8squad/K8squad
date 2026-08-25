// test/tickets/views.test.tsx — component-render units for the Tickets views
// (05-testing §3.2(b) blocked-badge, §3.2(d) List sort/tree, §3.2 8.17 tree).
//
// These cover what the pure-function units in derivation.test.ts cannot: that
// the List view renders the sub-ticket tree as TABLE-VALID <tr> siblings (a
// <div>/<ul> wrapper inside <tbody> is hoisted out by the browser and breaks the
// table), that carets/count-badges follow the 8.17 leaf-vs-parent rule, that
// expanding reveals child rows, and that the Kanban blocked overlay renders in
// the item's own lane (§8.6).

import { describe, it, expect, afterEach, vi } from "vitest";
import { render, screen, cleanup, fireEvent, within } from "@testing-library/react";
import { ListView } from "@/components/tickets/ListView";
import { KanbanBoard } from "@/components/tickets/KanbanBoard";
import type { TreeController } from "@/components/tickets/SubTicketTree";
import type { SortSpec, WorkItem } from "@/lib/tickets/types";

afterEach(cleanup);

function item(partial: Partial<WorkItem> & Pick<WorkItem, "id">): WorkItem {
  return {
    projectId: "p1",
    parentId: null,
    title: partial.title ?? `item ${partial.id}`,
    state: "backlog",
    blockedReason: null,
    updatedAt: "2026-08-01T00:00:00Z",
    ...partial,
  };
}

/** A controller with a fixed expanded set + preloaded children (no network). */
function controller(opts: {
  expanded?: Set<string>;
  children?: Record<string, WorkItem[]>;
  toggle?: (id: string) => void;
}): TreeController {
  const children = opts.children ?? {};
  const expanded = opts.expanded ?? new Set<string>();
  return {
    childrenOf: (pid) => children[pid],
    childCountOf: (it) => it.childCount ?? children[it.id]?.length ?? 0,
    isExpanded: (pid) => expanded.has(pid),
    toggle: opts.toggle ?? (() => {}),
  };
}

const SORT: SortSpec = { key: "updated", dir: "desc" };

describe("ListView — table-valid sub-ticket tree (8.14c + 8.17)", () => {
  it("renders every node as a <tr> — nothing is hoisted out of <tbody>", () => {
    const parent = item({ id: "par", title: "Parent", childCount: 1 });
    const child = item({ id: "kid", title: "Child", parentId: "par" });
    render(
      <ListView
        items={[parent]}
        tree={controller({ expanded: new Set(["par"]), children: { par: [child] } })}
        sort={SORT}
        onSortChange={vi.fn()}
      />,
    );
    const tbody = screen.getByTestId("row-par").closest("tbody")!;
    // Only <tr> may be a direct child of <tbody>; a stray <div>/<ul> here means
    // the browser hoisted the tree out and the table is broken.
    for (const el of Array.from(tbody.children)) {
      expect(el.tagName).toBe("TR");
    }
    // The expanded child renders as its own sibling row.
    expect(screen.getByTestId("row-kid")).toBeInTheDocument();
  });

  it("a parent shows a caret + child-count badge; a leaf shows neither (8.17 AC1)", () => {
    render(
      <ListView
        items={[item({ id: "par", childCount: 3 }), item({ id: "leaf" })]}
        tree={controller({})}
        sort={SORT}
        onSortChange={vi.fn()}
      />,
    );
    expect(screen.getByTestId("tree-caret-par")).toBeInTheDocument();
    expect(screen.getByTestId("child-count-par")).toHaveTextContent("3");
    expect(screen.queryByTestId("tree-caret-leaf")).toBeNull();
  });

  it("collapsed parent hides its children until expanded", () => {
    const parent = item({ id: "par", childCount: 1 });
    const child = item({ id: "kid", parentId: "par" });
    const { rerender } = render(
      <ListView
        items={[parent]}
        tree={controller({ children: { par: [child] } })}
        sort={SORT}
        onSortChange={vi.fn()}
      />,
    );
    expect(screen.queryByTestId("row-kid")).toBeNull();
    rerender(
      <ListView
        items={[parent]}
        tree={controller({ expanded: new Set(["par"]), children: { par: [child] } })}
        sort={SORT}
        onSortChange={vi.fn()}
      />,
    );
    expect(screen.getByTestId("row-kid")).toBeInTheDocument();
  });

  it("clicking a column header requests the toggled sort spec (8.14c AC2)", () => {
    const onSortChange = vi.fn();
    render(
      <ListView
        items={[item({ id: "a" })]}
        tree={controller({})}
        sort={SORT}
        onSortChange={onSortChange}
      />,
    );
    fireEvent.click(screen.getByTestId("sort-title"));
    expect(onSortChange).toHaveBeenCalledWith({ key: "title", dir: "asc" });
  });
});

describe("KanbanBoard — blocked overlay in-lane, RBAC drag gate (8.14b)", () => {
  const noop = vi.fn(async () => {});

  it("a blocked item renders in its workflow lane with a Blocked badge (§8.6)", () => {
    render(
      <KanbanBoard
        items={[item({ id: "b", state: "in_progress", blockedReason: "needs_approval" })]}
        tree={controller({})}
        role="viewer"
        onTransition={noop}
      />,
    );
    const lane = screen.getByTestId("column-in_progress");
    expect(within(lane).getByTestId("card-b")).toBeInTheDocument();
    expect(within(lane).getByTestId("blocked-badge-b")).toBeInTheDocument();
  });

  it("a viewer gets NO drag and NO quick-move control (UI RBAC gate)", () => {
    render(
      <KanbanBoard
        items={[item({ id: "v", state: "todo" })]}
        tree={controller({})}
        role="viewer"
        onTransition={noop}
      />,
    );
    expect(screen.getByTestId("card-v").getAttribute("draggable")).toBe("false");
    expect(screen.queryByTestId("quick-move-v")).toBeNull();
  });

  it("a contributor gets the quick-move control and a draggable card", () => {
    render(
      <KanbanBoard
        items={[item({ id: "c", state: "todo" })]}
        tree={controller({})}
        role="contributor"
        onTransition={noop}
      />,
    );
    expect(screen.getByTestId("card-c").getAttribute("draggable")).toBe("true");
    expect(screen.getByTestId("quick-move-c")).toBeInTheDocument();
  });
});
