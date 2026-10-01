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

/** A composer trigger char — `@` (agent mention) or `#` (ticket picker). */
export type MentionTrigger = "@" | "#";

/** The live trigger + fragment ending at the caret (ISI-5167). */
export interface TriggerFragment {
  trigger: MentionTrigger;
  /** The running fragment after the trigger char (the trigger itself stripped). */
  fragment: string;
}

/**
 * Like {@link mentionFragmentBefore} but detects EITHER trigger — `@` (agent
 * mention) or `#` (ticket picker, ISI-5167) — returning the trigger char and
 * the running fragment, or null when the caret is not in one. Same opening rule
 * (start-of-body or a non-word char before the trigger, so `a@b`/`c#3` mid-token
 * never fire) and the same `[A-Za-z0-9_-]` charset. The discussion composer uses
 * this because it supports both triggers; the ticket-detail composer stays
 * `@`-only via {@link mentionFragmentBefore}.
 */
export function triggerFragmentBefore(
  text: string,
  caret: number,
): TriggerFragment | null {
  const before = text.slice(0, caret);
  const m = /(^|[^A-Za-z0-9_-])([@#])([A-Za-z0-9_-]*)$/.exec(before);
  return m ? { trigger: m[2] as MentionTrigger, fragment: m[3] } : null;
}

/**
 * Replace the trailing `@`/`#` fragment in `before` with a canonical token,
 * choosing the prefix from `kind`: an agent → `@displayName `, a work item →
 * `#displayName ` (ISI-5167). It matches EITHER trigger char, so a `work_item`
 * picked from the `@` list still lands as `#…` — fixing the prior bug where a
 * ticket suggestion inserted with an `@` prefix.
 */
export function replaceTriggerFragment(
  before: string,
  displayName: string,
  kind: "agent" | "work_item",
): string {
  const prefix = kind === "work_item" ? "#" : "@";
  return before.replace(/[@#]([A-Za-z0-9_-]*)$/, `${prefix}${displayName} `);
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

/**
 * Resolve the `@name` tokens actually TYPED in a submitted body against the
 * squad roster, returning the DISTINCT canonical agent names in first-seen order
 * (ISI-5281). This is the body-side counterpart to {@link agentMentionSuggestions}
 * (the live autocomplete filter): the ticket composer dispatches from what the
 * human wrote, not from a dropdown pick — so a hand-typed `@mary` dispatches just
 * like one chosen from the popover, and the popover becomes pure autocomplete.
 *
 * Token grammar mirrors {@link mentionFragmentBefore} / {@link replaceMentionFragment}
 * exactly: a `@` at the start of the body or after a non-word char, then the
 * `[A-Za-z0-9_-]` mention charset — so an email's `a@b` is never a mention.
 * Resolution is a case-insensitive match on the agent NAME (the identity dispatch
 * resolves against), so `@Mary`/`@mary` both land on the roster's canonical
 * `mary`. A token matching no roster agent is ignored — it stays plain prose.
 */
export function resolveBodyMentions(
  body: string,
  agents: readonly AgentOption[],
): string[] {
  const canonicalByLower = new Map(
    agents.map((a) => [a.name.toLowerCase(), a.name] as const),
  );
  const re = /(^|[^A-Za-z0-9_-])@([A-Za-z0-9_-]+)/g;
  const seen = new Set<string>();
  const out: string[] = [];
  for (let m = re.exec(body); m !== null; m = re.exec(body)) {
    const canonical = canonicalByLower.get(m[2].toLowerCase());
    if (canonical !== undefined && !seen.has(canonical)) {
      seen.add(canonical);
      out.push(canonical);
    }
  }
  return out;
}
