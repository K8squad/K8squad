// Pure helpers backing the Discussions history panel (ISI-5673 / ISI-5598 WS-C).
//
// The left "Discussions" panel surfaces the EXISTING multi-thread backend (a
// "chat" === a `discussion.thread`; the list read is GET
// `/api/projects/{id}/discussion/threads`). The thread list carries only
// `createdAt` on the wire (internal/discussion/store.go — there is no
// last-activity/updatedAt column), so the row timestamp honestly reflects
// thread creation, and recency ordering mirrors the server's `ORDER BY
// created_at DESC`. Keeping the ordering/selection/formatting logic here (pure,
// `now`-injectable) lets the component stay thin and the behaviour unit-tested.

import type { Thread } from "./types";

/**
 * A compact, muted "last-activity" label for a thread row (mono, per the mock).
 * Backed by `createdAt` — the only timestamp the thread-list read exposes.
 * `now` is injected so the formatting is deterministic under test.
 */
export function relativeActivity(iso: string, now: number): string {
  const t = new Date(iso).getTime();
  if (!Number.isFinite(t)) return "";
  const diff = Math.floor((now - t) / 1000);
  if (diff < 0) return "just now";
  if (diff < 60) return `${diff}s ago`;
  if (diff < 3600) return `${Math.floor(diff / 60)}m ago`;
  if (diff < 86400) return `${Math.floor(diff / 3600)}h ago`;
  return `${Math.floor(diff / 86400)}d ago`;
}

/**
 * Newest-first by `createdAt` (mirrors the server list order), stable for ties.
 * Applied client-side so an optimistically-inserted new chat lands at the top
 * regardless of how the raw list arrived.
 */
export function sortThreadsByRecency(threads: readonly Thread[]): Thread[] {
  return [...threads]
    .map((t, i) => ({ t, i }))
    .sort((a, b) => {
      const ta = new Date(a.t.createdAt).getTime();
      const tb = new Date(b.t.createdAt).getTime();
      if (tb !== ta) return tb - ta;
      return a.i - b.i; // stable
    })
    .map(({ t }) => t);
}

/**
 * Resolve the thread the room should show from the `?thread={id}` URL request
 * and the loaded list. A requested id that exists in the list wins (linkable /
 * back-button friendly); otherwise the newest thread is the default; `null`
 * when the project has no threads yet (the empty-room bootstrap then opens one).
 */
export function resolveActiveThreadId(
  threads: readonly Thread[],
  requested: string | null | undefined,
): string | null {
  if (requested && threads.some((t) => t.id === requested)) return requested;
  return threads[0]?.id ?? null;
}

/**
 * Insert or replace a thread by id and return the list re-sorted newest-first.
 * Used for the optimistic new-chat insert (and to adopt the server's canonical
 * record once the POST resolves).
 */
export function upsertThread(
  threads: readonly Thread[],
  thread: Thread,
): Thread[] {
  const rest = threads.filter((t) => t.id !== thread.id);
  return sortThreadsByRecency([thread, ...rest]);
}
