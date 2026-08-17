// Threaded rendering — adjacency (`parentId`) → tree, the same shape the 8.x
// tree renders use (Story 10.3 AC1). Pure and deterministic: roots and replies
// are ordered by `createdAt` then `id` so the render is stable across
// re-fetches and live appends. A message whose `parentId` points outside the
// provided set is treated as a root (never dropped — audit-honest).

import type { Message } from "./types";

function byCreatedThenId(a: Message, b: Message): number {
  if (a.createdAt !== b.createdAt) return a.createdAt < b.createdAt ? -1 : 1;
  return a.id < b.id ? -1 : a.id > b.id ? 1 : 0;
}

/**
 * Nest a flat message list into a thread tree by `parentId`. Returns root
 * messages, each with `replies` populated recursively. Input is not mutated.
 */
export function nestMessages(flat: readonly Message[]): Message[] {
  // Clone so we never mutate caller state; reset any pre-existing `replies`.
  const nodes = new Map<string, Message>();
  for (const m of flat) {
    nodes.set(m.id, { ...m, replies: [] });
  }

  const roots: Message[] = [];
  for (const m of flat) {
    const node = nodes.get(m.id)!;
    const parent = m.parentId ? nodes.get(m.parentId) : undefined;
    if (parent) {
      parent.replies!.push(node);
    } else {
      // No parent, or parent not in this set → a root.
      roots.push(node);
    }
  }

  const sortRec = (list: Message[]) => {
    list.sort(byCreatedThenId);
    for (const n of list) if (n.replies?.length) sortRec(n.replies);
  };
  sortRec(roots);
  return roots;
}

/** Count all messages in a thread subtree (root + all nested replies). */
export function countThread(root: Message): number {
  let n = 1;
  for (const r of root.replies ?? []) n += countThread(r);
  return n;
}
