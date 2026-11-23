// lib/discussion/threadDigest.ts — the pure pre-seed builder for the
// create-tickets-from-thread flow (ISI-7710 / ISI-7681 P1; UX flow spec
// ISI-7709 §3). No React, no fetch: the sheet's Title + Markdown digest are
// derived CLIENT-SIDE from the already-loaded transcript (no refetch, no
// in-transcript selection UI in MVP), so this stays unit-testable on its own
// exactly like lib/discussion/slash.ts.
//
// The digest takes the LAST few NON-MARKER messages (markers — reset,
// proposal, structured post-backs — are coordination chrome, not content a
// ticket should cite) and never a retracted bubble. The selected ids ride the
// draft as sourceMessageIds so every created work item keeps its provenance
// backlink to the citing messages (ISI-7708 labels).

import type { Message } from "./types";

/** The pre-seed the sheet opens with (UX spec §3). */
export interface ThreadDigest {
  /** Seeded from the thread title; the human edits before confirming. */
  title: string;
  /** A short Markdown digest of the recent non-marker messages. */
  body: string;
  /** Ids of the digested messages — the draft's provenance backlink set. */
  sourceMessageIds: string[];
}

/** How many recent messages the digest cites (UX spec §3: "last ~5"). */
const DIGEST_MESSAGES = 5;
/** Title seed cap — a thread title can be long; a ticket title should not be. */
const TITLE_MAX = 80;

/** Markers are coordination chrome (reset/proposal/structured), not content. */
function isMarker(m: Message): boolean {
  if (m.invalidatedAt) return true;
  return m.kind !== undefined && m.kind !== "text";
}

/** First line of a body, trimmed — the digest cites topics, not walls. */
function firstLine(s: string): string {
  const t = s.trim();
  const i = t.indexOf("\n");
  return (i === -1 ? t : t.slice(0, i)).trim();
}

/**
 * Build the sheet pre-seed from a thread title and its FLAT loaded messages.
 * Pure and total: any input yields a usable seed (empty threads yield an
 * empty body + no ids; the entry is disabled upstream anyway).
 */
export function buildThreadDigest(
  threadTitle: string | undefined,
  messages: readonly Message[],
): ThreadDigest {
  const recent = messages.filter((m) => !isMarker(m)).slice(-DIGEST_MESSAGES);
  const lines = recent.map(
    (m) => `- **${m.authorPrincipal}**: ${firstLine(m.body)}`,
  );
  return {
    title: (threadTitle ?? "").trim().slice(0, TITLE_MAX),
    body: lines.length > 0 ? `Thread summary:\n${lines.join("\n")}` : "",
    sourceMessageIds: recent.map((m) => m.id).slice(0, 19),
  };
}
