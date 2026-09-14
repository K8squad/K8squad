// lib/tickets/thread.ts — the ticket-detail (S3 / ISI-4399) read model: the
// browser-side client + pure shaping helpers for GET /api/work-items/{id}, which
// the apiserver answers with coord.WorkItemThread (workitemread.go) — the full
// ticket: description, agent-authored comments, the §6.5 status-change history,
// and the agent-reported change refs (commits / PR links).
//
// WIRE CASING: coord.TaskDetail is embedded WITHOUT json tags, so its fields
// marshal PascalCase (Title, Description, State, Comments, ChangeRefs, Holder,
// RunID…) while the nested arrays and StatusHistory carry lowercase tags. That
// mixed shape is ugly and one add-a-tag away from drifting, so normalizeThread
// is the SINGLE tolerant boundary — it reads either casing and hands the rest of
// the UI one clean camelCase shape. Everything downstream consumes NormalizedThread.

import { ApiError } from "./api";
import type { WorkItemState } from "./types";

/** One append-only comment (coord.comment) — author principal + body + time. */
export interface ThreadComment {
  author: string;
  body: string;
  createdAt: string;
}

/** One agent-reported change ref (coord.change_ref) — commit SHA or PR URL. */
export interface ThreadChangeRef {
  kind: string;
  ref: string;
  summary?: string;
  author: string;
  runId?: string;
  createdAt: string;
}

/** One lane move from the §6.5 audit history. */
export interface ThreadStatusChange {
  fromState: string;
  toState: string;
  principal: string;
  occurredAt: string;
}

/** The clean, camelCase ticket-detail shape the UI renders. */
export interface NormalizedThread {
  workItemId: string;
  title: string;
  description: string;
  state: WorkItemState | string;
  blockedReason: string;
  comments: ThreadComment[];
  changeRefs: ThreadChangeRef[];
  statusHistory: ThreadStatusChange[];
  /** Current claim holder principal ("" ⇒ unclaimed) + holding run, for the sidebar. */
  holder: string;
  runId: string;
}

// Casing-tolerant readers — accept the server's PascalCase OR a future camelCase.
function str(raw: Record<string, unknown>, ...keys: string[]): string {
  for (const k of keys) {
    const v = raw[k];
    if (typeof v === "string") return v;
  }
  return "";
}

function arr(raw: Record<string, unknown>, ...keys: string[]): unknown[] {
  for (const k of keys) {
    const v = raw[k];
    if (Array.isArray(v)) return v;
  }
  return [];
}

/** Map the raw wire object (either casing) into the clean shape. */
export function normalizeThread(raw: unknown): NormalizedThread {
  const r = (raw ?? {}) as Record<string, unknown>;
  const comments = arr(r, "Comments", "comments").map((c) => {
    const o = c as Record<string, unknown>;
    return {
      author: str(o, "author", "Author"),
      body: str(o, "body", "Body"),
      createdAt: str(o, "createdAt", "CreatedAt"),
    };
  });
  const changeRefs = arr(r, "ChangeRefs", "changeRefs").map((c) => {
    const o = c as Record<string, unknown>;
    const summary = str(o, "summary", "Summary");
    const runId = str(o, "runId", "RunID");
    return {
      kind: str(o, "kind", "Kind"),
      ref: str(o, "ref", "Ref"),
      ...(summary ? { summary } : {}),
      author: str(o, "author", "Author"),
      ...(runId ? { runId } : {}),
      createdAt: str(o, "createdAt", "CreatedAt"),
    };
  });
  const statusHistory = arr(r, "statusHistory", "StatusHistory").map((c) => {
    const o = c as Record<string, unknown>;
    return {
      fromState: str(o, "fromState", "FromState"),
      toState: str(o, "toState", "ToState"),
      principal: str(o, "principal", "Principal"),
      occurredAt: str(o, "occurredAt", "OccurredAt"),
    };
  });
  return {
    workItemId: str(r, "WorkItemID", "workItemId", "id"),
    title: str(r, "Title", "title"),
    description: str(r, "Description", "description", "body"),
    state: str(r, "State", "state"),
    blockedReason: str(r, "BlockedReason", "blockedReason"),
    comments,
    changeRefs,
    statusHistory,
    holder: str(r, "Holder", "holder"),
    runId: str(r, "RunID", "runId"),
  };
}

/** Fetch + normalize one ticket's thread via the BFF (throws ApiError on non-2xx). */
export async function fetchWorkItemThread(
  workItemId: string,
): Promise<NormalizedThread> {
  const res = await fetch(`/api/work-items/${encodeURIComponent(workItemId)}`, {
    cache: "no-store",
  });
  const text = await res.text();
  if (!res.ok) throw new ApiError(res.status, text);
  let raw: unknown;
  try {
    raw = JSON.parse(text);
  } catch {
    throw new ApiError(res.status, text);
  }
  return normalizeThread(raw);
}

// ---------------------------------------------------------------------------
// Pure activity-timeline shaping (unit-tested — no fetch, no DOM).
// ---------------------------------------------------------------------------

export type ActivityKind = "comment" | "event" | "change";

/** One unified row in the chronological Activity thread (design §3). */
export interface ActivityItem {
  kind: ActivityKind;
  at: string;
  /** author/principal for comment & change; "" for a bare event. */
  who: string;
  /** the human-role of `who` — a comment/change from an agent vs a person. */
  authorKind: "agent" | "user";
  comment?: ThreadComment;
  change?: ThreadChangeRef;
  event?: ThreadStatusChange;
}

/** A principal like "agent:builder" is an agent; anything else is a person. */
export function authorKind(principal: string): "agent" | "user" {
  return principal.startsWith("agent:") || principal.startsWith("agent/")
    ? "agent"
    : "user";
}

/**
 * Merge comments + status changes + change refs into ONE chronological list,
 * NEWEST LAST (design §3 "chronological, newest last"). A missing/unparseable
 * timestamp sorts to the end (0 epoch would wrongly float to the top of an
 * ascending sort, so treat it as +∞ instead — an undated row is "just now").
 */
export function buildActivity(thread: NormalizedThread): ActivityItem[] {
  const items: ActivityItem[] = [];
  for (const c of thread.comments) {
    items.push({
      kind: "comment",
      at: c.createdAt,
      who: c.author,
      authorKind: authorKind(c.author),
      comment: c,
    });
  }
  for (const e of thread.statusHistory) {
    items.push({
      kind: "event",
      at: e.occurredAt,
      who: e.principal,
      authorKind: authorKind(e.principal),
      event: e,
    });
  }
  for (const c of thread.changeRefs) {
    items.push({
      kind: "change",
      at: c.createdAt,
      who: c.author,
      authorKind: authorKind(c.author),
      change: c,
    });
  }
  const at = (s: string) => {
    const t = Date.parse(s);
    return Number.isNaN(t) ? Number.POSITIVE_INFINITY : t;
  };
  return items.sort((a, b) => at(a.at) - at(b.at));
}

/** Sub-ticket rollup for the "N of M done" progress line (design §3). */
export function subTicketProgress(children: { state: string }[]): {
  done: number;
  total: number;
} {
  let done = 0;
  for (const c of children) if (c.state === "done") done += 1;
  return { done, total: children.length };
}
