// lib/mentions.ts — the shared `@`-mention primitives (ISI-5159).
//
// The discussion-room composer (ISI-4926/ISI-4929) grew a `@`-trigger popover
// with a fragment matcher and a token-insert helper; ISI-5159 brings the SAME
// affordance to the ticket-detail comment composer so a human can `@`-mention an
// agent and have it assigned/dispatched onto the ticket (the board's "same
// experience we have in Paperclip"). Rather than clone the regex into a second
// component, the matcher + inserter live here and both composers import them.
//
// These are pure string helpers — no React, no fetch — so they are trivially
// unit-testable and carry no feature coupling. The discussion popover UI
// (`components/discussion/MentionPopover.tsx`) and the `MentionSuggestion` wire
// type stay the shared rendering/data contract on top of them.

import type { MentionSuggestion } from "@/lib/discussion/types";
import type { AgentOption } from "@/lib/tickets/api";

/**
 * The live `@fragment` ending at `caret`, or null when the caret is not in one.
 * A fragment opens on a `@` that is at the start of the body or preceded by a
 * non-word char (so an email's `a@b` never triggers), and runs over the mention
 * charset `[A-Za-z0-9_-]`. Returns the fragment WITHOUT the leading `@`.
 */
export function mentionFragmentBefore(
  text: string,
  caret: number,
): string | null {
  const before = text.slice(0, caret);
  const m = /(^|[^A-Za-z0-9_-])@([A-Za-z0-9_-]*)$/.exec(before);
  return m ? m[2] : null;
}

/**
 * Replace the trailing `@fragment` in `before` (the text up to the caret) with
 * the canonical `@displayName ` token, returning the new `before` slice. The
 * caller concatenates it with the untouched `after` slice and repositions the
 * caret to `result.length`.
 */
export function replaceMentionFragment(
  before: string,
  displayName: string,
): string {
  return before.replace(/@([A-Za-z0-9_-]*)$/, `@${displayName} `);
}

/**
 * Project the loaded squad roster into `MentionSuggestion`s filtered by the live
 * fragment (ISI-5159). The ticket composer already holds the roster it uses for
 * the "Assign to…" select (`listSquadAgents`), so the `@`-popover filters that
 * list CLIENT-SIDE — no new backend surface, and every suggestion maps 1:1 to a
 * dispatchable agent NAME (the identity `POST .../dispatch {agentId}` resolves).
 * The match is a case-insensitive substring on the agent name; an empty fragment
 * offers the whole roster (the `@` alone opens the picker).
 */
export function agentMentionSuggestions(
  agents: readonly AgentOption[],
  fragment: string,
): MentionSuggestion[] {
  const needle = fragment.toLowerCase();
  return agents
    .filter((a) => needle === "" || a.name.toLowerCase().includes(needle))
    .map((a) => ({
      type: "agent" as const,
      id: a.name,
      displayName: a.name,
      state: a.role,
    }));
}
