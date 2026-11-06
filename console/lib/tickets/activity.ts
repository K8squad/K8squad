// lib/tickets/activity.ts — the read + pure shaping layer behind the two-phase
// comment UI's per-run "Activity (N events)" expander (ISI-7095 / ISI-7088).
//
// ISI-7094 split the coord.comment thread in two: the default read
// (GET /api/work-items/{id}) now carries only the human conversation + the ONE
// per-run summary, while the run sub-step firehose the operator's ProgressMirror
// mirrors ([run …][tool]…[status], LLM digests) is tagged kind='progress' and
// served SEPARATELY from GET …/activity. This module fetches that progress feed
// and groups it per run, chronologically, so the ticket view can collapse each
// run's log under one expander instead of burying the summary under the firehose.
//
// PURE shaping (groupActivityByRun) + a thin fetch (fetchRunActivity): the DOM
// renderer (TicketDetail) consumes RunActivityGroup[] and owns nothing about the
// wire. The feed is fetched LAZILY — only when a run's expander first opens — so
// a ticket nobody expands never pays for the rows.

import { ApiError } from "./api";
import type { ThreadComment } from "./thread";
import { commentKind } from "./thread";

/** One run's collapsed progress log — the backing of a single expander. */
export interface RunActivityGroup {
  /** The run id these events belong to; "" ⇒ the legacy ungrouped bucket
   *  (progress rows a pre-0042 server wrote without a run id). */
  runId: string;
  /** Short, stable label for the expander header (e.g. "abcd1234"); "earlier"
   *  for the legacy bucket, which carries no run id to name. */
  label: string;
  /** The run's progress rows, chronological (oldest first) — the order the feed
   *  already arrives in from the backend (created_at ASC). */
  events: ThreadComment[];
  /** The group's chronological anchor — its FIRST event's time — used only to
   *  order the runs against each other (design §3: "runs grouped chronologically"). */
  at: string;
}

/** The short run label shown on an expander header: the run id's leading token,
 *  matching the `run/<shortRunID>` the ProgressMirror authors (first 8 chars of
 *  the Run UID). A legacy (run-id-less) bucket is labelled "earlier". */
export function runLabel(runId: string): string {
  const id = (runId ?? "").trim();
  if (id === "") return "earlier";
  const head = id.split("-")[0] ?? id;
  return head.length > 8 ? head.slice(0, 8) : head;
}

const epoch = (s: string) => {
  const t = Date.parse(s);
  // An undated row sorts to the end (a 0-epoch would float to the top of an
  // ascending sort), matching buildActivity's +∞ convention.
  return Number.isNaN(t) ? Number.POSITIVE_INFINITY : t;
};

/**
 * Group the progress feed into per-run buckets, the runs ordered CHRONOLOGICALLY
 * by their first event and each bucket's events kept oldest-first (design §3).
 * Rows without a run id collapse into ONE trailing "earlier" bucket so a legacy
 * ticket still reads, rather than scattering one bucket per orphaned row. Pure
 * and input-order-independent: the caller may hand it the feed in any order.
 */
export function groupActivityByRun(
  progress: readonly ThreadComment[],
): RunActivityGroup[] {
  const byRun = new Map<string, ThreadComment[]>();
  for (const c of progress) {
    const key = c.runId ?? "";
    const bucket = byRun.get(key);
    if (bucket) bucket.push(c);
    else byRun.set(key, [c]);
  }

  const groups: RunActivityGroup[] = [];
  for (const [runId, rawEvents] of byRun) {
    const events = [...rawEvents].sort(
      (a, b) => epoch(a.createdAt) - epoch(b.createdAt),
    );
    groups.push({
      runId,
      label: runLabel(runId),
      events,
      at: events.length ? events[0].createdAt : "",
    });
  }

  // Runs ordered by their first event; the legacy ("") bucket has no run and
  // trails last regardless of its timestamps — it is the catch-all, not a run.
  return groups.sort((a, b) => {
    if (a.runId === "" && b.runId !== "") return 1;
    if (b.runId === "" && a.runId !== "") return -1;
    return epoch(a.at) - epoch(b.at);
  });
}

/** Total progress events across all runs — the "(N events)" count. */
export function activityEventCount(groups: readonly RunActivityGroup[]): number {
  return groups.reduce((n, g) => n + g.events.length, 0);
}

/**
 * Fetch one ticket's PROGRESS feed via the BFF (GET …/activity), normalized into
 * the SAME ThreadComment shape the thread read emits. Throws ApiError on non-2xx
 * (the caller surfaces 404/501 as "no activity on this deployment", never a
 * crash). A store-less deployment answers 501 → the expander renders empty.
 */
export async function fetchRunActivity(
  workItemId: string,
): Promise<ThreadComment[]> {
  const res = await fetch(
    `/api/work-items/${encodeURIComponent(workItemId)}/activity`,
    { cache: "no-store" },
  );
  const text = await res.text();
  if (!res.ok) throw new ApiError(res.status, text);
  let raw: unknown;
  try {
    raw = JSON.parse(text);
  } catch {
    throw new ApiError(res.status, text);
  }
  if (!Array.isArray(raw)) return [];
  return raw.map((entry) => {
    const o = (entry ?? {}) as Record<string, unknown>;
    const str = (...keys: string[]) => {
      for (const k of keys) {
        const v = o[k];
        if (typeof v === "string") return v;
      }
      return "";
    };
    const kind = commentKind(o);
    const runId = str("runId", "RunID");
    return {
      author: str("author", "Author"),
      body: str("body", "Body"),
      createdAt: str("createdAt", "CreatedAt"),
      ...(kind ? { kind } : {}),
      ...(runId ? { runId } : {}),
    };
  });
}
