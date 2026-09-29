"use client";

// Composer — post a new message or reply-in-thread (AC3). It collects a body,
// an optional in-thread parent, and — since the Discussion Room v2 wire
// widening (ISI-4929, plan §4.1/§4.2) — an optional AUDIENCE: `party` (the
// default, board OQ2) or a direct target picked from the room roster. On
// submit it hands the input to `buildPostBody`, which is the single choke
// point guaranteeing the wire body is `{ body, parentId?, audience? }` and
// NOTHING else — provenance is server-stamped.
//
// @-mentions and #-tickets (plan §4.3, ISI-5167): typing `@` or `#` + a
// fragment pops the MentionPopover, backed by the injected `searchMentions`
// (the ISI-4926 endpoint, which returns both agent AND project-scoped
// work_item suggestions). The trigger scopes the picker: `#` shows only
// work_item (ticket) suggestions; `@` shows the full set (agents lead). Picking
// an AGENT replaces the fragment with the `@Name` mention token; picking a
// WORK_ITEM replaces it with the `#Title` ticket token AND collects a
// structured link ({ workItemId, title }) into the outgoing payload — a LINK,
// never a dispatch (ISI-5165 resolves/drops it server-side). This also fixes
// the prior bug where a work_item picked from the `@` list inserted an `@`
// prefix. When no `searchMentions` prop is wired the composer never pops.
//
// Still a COLLABORATION surface only: no custody verb rides this form.

import { useEffect, useRef, useState } from "react";
import {
  buildPostBody,
  canSubmit,
  type ComposerInput,
  type PostMessageBody,
  type TicketReference,
} from "@/lib/discussion/compose";
import type { Audience } from "@/lib/discussion/audience";
import type { MentionSuggestion } from "@/lib/discussion/types";
import {
  mentionFragmentBefore,
  triggerFragmentBefore,
  replaceTriggerFragment,
  type MentionTrigger,
} from "@/lib/mentions";
import { MentionPopover } from "./MentionPopover";

// The `@`-fragment matcher moved to the shared `lib/mentions` primitive (ISI-5159)
// so the ticket composer reuses the exact same trigger semantics; re-exported here
// for callers that still import it from this module.
export { mentionFragmentBefore };

/** A direct-target option for the audience selector (one roster agent). */
export interface DirectTarget {
  id: string;
  name: string;
}

export interface ComposerProps {
  /** When set, this composer replies in-thread to the given parent message. */
  parentId?: string | null;
  /** Receives the exact wire body — `{ body, parentId?, audience? }`, never any author. */
  onPost: (body: PostMessageBody) => void | Promise<void>;
  placeholder?: string;
  /**
   * Roster agents offered as direct targets (ISI-4929). Absent/empty ⇒ no
   * audience selector renders and every post is party (the default).
   */
  directTargets?: readonly DirectTarget[];
  /** Mention search backing the `@` popover (ISI-4926 endpoint). */
  searchMentions?: (q: string) => Promise<MentionSuggestion[]>;
}

const MENTION_DEBOUNCE_MS = 200;

export function Composer({
  parentId,
  onPost,
  placeholder,
  directTargets,
  searchMentions,
}: ComposerProps) {
  const [body, setBody] = useState("");
  const [audience, setAudience] = useState<Audience>({ kind: "party" });
  const [suggestions, setSuggestions] = useState<MentionSuggestion[]>([]);
  const [mentionLoading, setMentionLoading] = useState(false);
  const [activeIndex, setActiveIndex] = useState(0);
  const [mentionOpen, setMentionOpen] = useState(false);
  const [fragment, setFragment] = useState("");
  // Which trigger opened the popover: `@` shows the full result set (agents
  // lead), `#` scopes it to work_item (ticket) suggestions (ISI-5167).
  const [trigger, setTrigger] = useState<MentionTrigger>("@");
  // Ticket links the `#` picker collected — carried into the outgoing payload
  // (a LINK, not a dispatch; ISI-5165 resolves/drops them server-side).
  const [refs, setRefs] = useState<TicketReference[]>([]);
  const caretRef = useRef(0);
  const textareaRef = useRef<HTMLTextAreaElement | null>(null);

  const input: ComposerInput = {
    body,
    parentId: parentId ?? null,
    audience,
    references: refs,
  };
  const disabled = !canSubmit(input);

  // Debounced mention query: fires only while a live trigger fragment is open.
  // A `#` trigger scopes the shown suggestions to work_item (ticket) rows.
  useEffect(() => {
    if (!mentionOpen || !searchMentions || fragment.length < 1) return;
    let alive = true;
    setMentionLoading(true);
    const t = setTimeout(() => {
      searchMentions(fragment)
        .then((results) => {
          if (!alive) return;
          const shown =
            trigger === "#"
              ? results.filter((r) => r.type === "work_item")
              : results;
          setSuggestions(shown);
          setActiveIndex(0);
          setMentionLoading(false);
        })
        .catch(() => {
          if (!alive) return;
          setSuggestions([]);
          setMentionLoading(false);
        });
    }, MENTION_DEBOUNCE_MS);
    return () => {
      alive = false;
      clearTimeout(t);
      setMentionLoading(false);
    };
  }, [fragment, mentionOpen, searchMentions, trigger]);

  const syncMention = (text: string, caret: number) => {
    caretRef.current = caret;
    const frag = triggerFragmentBefore(text, caret);
    if (frag === null || !searchMentions) {
      setMentionOpen(false);
      setFragment("");
      return;
    }
    setTrigger(frag.trigger);
    setFragment(frag.fragment);
    setMentionOpen(true);
  };

  const insertMention = (s: MentionSuggestion) => {
    const el = textareaRef.current;
    const caret = caretRef.current;
    const before = body.slice(0, caret);
    const after = body.slice(caret);
    // Agent → `@Name` mention; work_item → `#Title` ticket token (regardless of
    // which trigger the human typed — this also fixes the prior `@Title` bug).
    const newBefore = replaceTriggerFragment(before, s.displayName, s.type);
    setBody(newBefore + after);
    // A picked ticket carries its structured link (UUID + title) into the
    // payload; dedupe by work-item id so re-picking is a no-op.
    if (s.type === "work_item") {
      setRefs((prev) =>
        prev.some((r) => r.workItemId === s.id)
          ? prev
          : [...prev, { workItemId: s.id, title: s.displayName }],
      );
    }
    setMentionOpen(false);
    setFragment("");
    const nextCaret = newBefore.length;
    if (el) {
      el.focus();
      requestAnimationFrame(() => el.setSelectionRange(nextCaret, nextCaret));
    }
  };

  const onBodyKeyDown = (e: React.KeyboardEvent<HTMLTextAreaElement>) => {
    if (!mentionOpen) return;
    if (e.key === "Escape") {
      e.preventDefault();
      setMentionOpen(false);
      return;
    }
    if (suggestions.length === 0) return;
    if (e.key === "ArrowDown") {
      e.preventDefault();
      setActiveIndex((i) => (i + 1) % suggestions.length);
      return;
    }
    if (e.key === "ArrowUp") {
      e.preventDefault();
      setActiveIndex((i) => (i - 1 + suggestions.length) % suggestions.length);
      return;
    }
    if (e.key === "Enter" || e.key === "Tab") {
      e.preventDefault();
      insertMention(suggestions[activeIndex]);
    }
  };

  const submit = async () => {
    if (!canSubmit(input)) return;
    await onPost(buildPostBody(input));
    setBody("");
    setRefs([]);
    setMentionOpen(false);
  };

  return (
    <form
      className="ksq-composer"
      data-testid="composer"
      data-reply-to={parentId ?? ""}
      onSubmit={(e) => {
        e.preventDefault();
        void submit();
      }}
    >
      <div className="ksq-composer__row">
        {directTargets && directTargets.length > 0 ? (
          <select
            className="ksq-composer__audience"
            data-testid="audience-select"
            aria-label="Audience"
            value={audience.kind === "direct" ? audience.agentId : ""}
            onChange={(e) => {
              const v = e.target.value;
              setAudience(
                v ? { kind: "direct", agentId: v } : { kind: "party" },
              );
            }}
          >
            <option value="">Party (room)</option>
            {directTargets.map((t) => (
              <option key={t.id} value={t.id}>
                Direct · {t.name}
              </option>
            ))}
          </select>
        ) : null}
        <textarea
          ref={textareaRef}
          className="ksq-composer__body"
          data-testid="composer-body"
          aria-label={parentId ? "Reply in thread" : "Post a message"}
          placeholder={
            placeholder ?? (parentId ? "Reply…" : "Post to the room…")
          }
          value={body}
          onChange={(e) => {
            setBody(e.target.value);
            syncMention(e.target.value, e.target.selectionStart ?? 0);
          }}
          onKeyDown={onBodyKeyDown}
        />
        <button
          type="submit"
          className="ksq-composer__submit"
          data-testid="composer-submit"
          disabled={disabled}
        >
          {parentId ? "Reply" : "Post"}
        </button>
      </div>
      {mentionOpen ? (
        <MentionPopover
          suggestions={suggestions}
          activeIndex={activeIndex}
          loading={mentionLoading}
          onSelect={insertMention}
          onDismiss={() => {
            setMentionOpen(false);
            setFragment("");
          }}
        />
      ) : null}
    </form>
  );
}
