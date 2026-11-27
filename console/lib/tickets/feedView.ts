// lib/tickets/feedView.ts - ISI-7888 (parent ISI-7859, design ISI-7861 para-4):
// the conversation-first issue feed's pure shaping layer.
//
// The issue detail feed is a CONVERSATION, not a log: only things a person or
// an agent SAID render first-class; everything the machine did is evidence -
// grouped, collapsed, de-emphasized. This module owns the para-4 taxonomy
// rules and the Conversation/All-activity block assembly, DOM-free so the
// mapping is unit-testable:
//   - author human/agent (incl. run/<id> mirrors) -> comment (first-class)
//   - operator-settled recaps (kind='summary' with a non-agent author - the
//     run-failure notice, the synthesized no-summary recap) -> system notice
//   - lane/state-transition events -> runs of >=2 consecutive events collapse
//     into ONE activity group (conversation mode); one row each (all mode)
//   - live SSE thinking rows -> owned by the streaming panel on the owning
//     agent comment - never inline feed rows in conversation mode
//   - decision/confirmation items stay first-class (TicketDecisions - untouched)
// "All activity" ungroups every event for debugging; the tab choice persists
// per user (localStorage, the ProjectsNavTree rail pattern, ISI-7861 para-5).

import { authorKind, type ActivityItem, type ThreadComment } from "./thread";

export type FeedMode = "conversation" | "all";

/** localStorage key for the per-user Conversation/All-activity tab choice. */
export const FEED_MODE_STORAGE_KEY = "ksq.ticket.feedMode";

/** Read the stored tab choice; "conversation" (the design default) on any
 * miss - absent key, private mode, disabled storage, or a legacy value. */
export function readStoredFeedMode(): FeedMode {
  try {
    return localStorage.getItem(FEED_MODE_STORAGE_KEY) === "all"
      ? "all"
      : "conversation";
  } catch {
    return "conversation";
  }
}

/** Persist the tab choice (best-effort; storage failures are silent). */
export function storeFeedMode(mode: FeedMode): void {
  try {
    localStorage.setItem(FEED_MODE_STORAGE_KEY, mode);
  } catch {
    // best-effort persistence only
  }
}

/** One expandable affordance's persisted open state (per user, per
 * group/notice/panel - ISI-7861 para-5). Defaults closed. */
export function readStoredExpanded(scope: string): boolean {
  try {
    return localStorage.getItem(`ksq.ticket.feed.open.${scope}`) === "1";
  } catch {
    return false;
  }
}

export function storeExpanded(scope: string, open: boolean): void {
  try {
    localStorage.setItem(`ksq.ticket.feed.open.${scope}`, open ? "1" : "0");
  } catch {
    // best-effort persistence only
  }
}

/** Operator/system principal prefixes: the dispatcher Initiator vocabulary
 * ("system:ci-failure", "system:issue-triage", ...) plus the bare service
 * identities the console has historically rendered (ksquad-operator). */
const SYSTEM_AUTHOR = /^(?:system|operator|ksquad-operator)(?::|\/)/i;
const BARE_SYSTEM_AUTHORS = new Set(["ksquad-operator", "ksquad-apiserver"]);

/** True when a principal names the machine, not a person or an agent. */
export function isSystemPrincipal(principal: string): boolean {
  const p = (principal ?? "").trim();
  return SYSTEM_AUTHOR.test(p) || BARE_SYSTEM_AUTHORS.has(p.toLowerCase());
}

/** True when a thread comment is operator/system output rather than speech
 * (para-4 rule: author operator/system -> notice when it is an outcome a
 * person cares about). The operator's settle stamps its notices
 * kind='summary' with a NON-agent author; agents (incl. the run/<id> progress
 * mirror) author their own summaries and stay comments. Belt-and-braces:
 * known system principals and the failure-notice header also classify, so a
 * legacy or future producer that drops the kind stamp still de-noises. */
export function isSystemNoticeComment(comment: ThreadComment): boolean {
  if (authorKind(comment.author) === "agent") return false;
  if (comment.kind === "summary") return true;
  if (isSystemPrincipal(comment.author)) return true;
  return (comment.body ?? "").startsWith("Run failure notice");
}

/** The one-line headline for a collapsed system-notice card (para-5: warning
 * Run failed - model route). Cause classes mirror failurenotice.go's humane
 * classes so the card and the evidence never disagree. */
export function noticeHeadline(body: string): string {
  const text = body ?? "";
  if (/^Run failure notice/.test(text)) {
    if (/model route/i.test(text)) return "Run failed - model route";
    if (/follow stream/i.test(text)) return "Run failed - follow stream error";
    if (/sandbox death/i.test(text) || /sandbox died/i.test(text)) {
      return "Run failed - sandbox death";
    }
    if (/before it started/i.test(text)) {
      return "Run failed - died before start";
    }
    return "Run failed";
  }
  if (/^Run summary \(auto-generated/.test(text)) {
    return "Run summary - auto-generated";
  }
  // Unknown producer: its own first line, clipped, never a fabricated cause.
  const first = text.split("\n", 1)[0] ?? "";
  return first.length > 80
    ? `${first.slice(0, 77)}…`
    : first || "System notice";
}

/** The run id a notice's evidence line carries ("run_id=..."), "" when absent. */
export function noticeRunId(body: string): string {
  const m = /run_id=([0-9a-fA-F-]{8,})/.exec(body ?? "");
  return m ? (m[1] ?? "") : "";
}

/** One rendered block of the conversation-first feed. */
export type FeedBlock =
  /** Rendered by the existing ActivityRow (comments, change refs, lone
   * events, and - in All-activity mode - every single event row). */
  | { kind: "row"; item: ActivityItem }
  /** A system-notice card (operator outcome, collapsed one-liner). */
  | { kind: "notice"; item: ActivityItem }
  /** A collapsed activity group: >=2 consecutive lane/state events. */
  | { kind: "activity"; items: ActivityItem[] };

/** Assemble the feed blocks for a mode (para-4 + para-5):
 *  - conversation: runs of >=2 consecutive event rows collapse into ONE
 *    activity block; live thinking rows drop out (the streaming panel owns
 *    them); notices classify per isSystemNoticeComment;
 *  - all: every event renders as its own row (ungrouped, today's shape) and
 *    live thinking rows stay inline; notices keep their card treatment (the
 *    taxonomy's "never a comment card" is mode-independent).
 * Pure and input-order-preserving. */
export function buildFeed(
  items: readonly ActivityItem[],
  mode: FeedMode,
): FeedBlock[] {
  const blocks: FeedBlock[] = [];
  let pending: ActivityItem[] = [];
  const flush = () => {
    if (pending.length >= 2) {
      blocks.push({ kind: "activity", items: pending });
    } else if (pending.length === 1) {
      blocks.push({ kind: "row", item: pending[0] });
    }
    pending = [];
  };
  for (const item of items) {
    if (item.kind === "event") {
      if (mode === "conversation") pending.push(item);
      else {
        flush();
        blocks.push({ kind: "row", item });
      }
      continue;
    }
    flush();
    if (
      mode === "conversation" &&
      item.kind === "comment" &&
      item.live &&
      item.liveTier !== "deliverable"
    ) {
      // The streaming panel on the owning agent comment owns live steps.
      continue;
    }
    if (item.kind === "comment" && item.comment) {
      if (isSystemNoticeComment(item.comment)) {
        blocks.push({ kind: "notice", item });
        continue;
      }
    }
    blocks.push({ kind: "row", item });
  }
  flush();
  return blocks;
}
