"use client";

// Composer — post a new message or reply-in-thread (AC3). It collects a body,
// an optional in-thread parent, and — since the Discussion Room v2 wire
// widening (ISI-4929, plan §4.1/§4.2) — an optional AUDIENCE: `party` (the
// default, board OQ2) or a direct target picked from the room roster. On
// submit it hands the input to `buildPostBody`, which is the single choke
// point guaranteeing the wire body is `{ body, parentId?, audience? }` and
// NOTHING else — provenance is server-stamped.
//
// @-mentions (plan §4.3): typing `@` + a fragment pops the MentionPopover,
// backed by the injected `searchMentions` (the ISI-4926 endpoint). Picking a
// suggestion replaces the fragment with the `@Name` token at the caret. When
// no `searchMentions` prop is wired the composer simply never pops — the
// affordance degrades silently.
//
// Still a COLLABORATION surface only: no custody verb rides this form.

import { useEffect, useRef, useState } from "react";
import {
  buildPostBody,
  canSubmit,
  type ComposerInput,
  type PostMessageBody,
} from "@/lib/discussion/compose";
import type { Audience } from "@/lib/discussion/audience";
import type { MentionSuggestion } from "@/lib/discussion/types";
import { MentionPopover } from "./MentionPopover";

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

/** The @-fragment ending at `caret`, or null when the caret is not in one. */
export function mentionFragmentBefore(
  text: string,
  caret: number,
): string | null {
  const before = text.slice(0, caret);
  const m = /(^|[^A-Za-z0-9_-])@([A-Za-z0-9_-]*)$/.exec(before);
  return m ? m[2] : null;
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
  const caretRef = useRef(0);
  const textareaRef = useRef<HTMLTextAreaElement | null>(null);

  const input: ComposerInput = { body, parentId: parentId ?? null, audience };
  const disabled = !canSubmit(input);

  // Debounced mention query: fires only while a live `@fragment` is open.
  useEffect(() => {
    if (!mentionOpen || !searchMentions || fragment.length < 1) return;
    let alive = true;
    setMentionLoading(true);
    const t = setTimeout(() => {
      searchMentions(fragment)
        .then((results) => {
          if (!alive) return;
          setSuggestions(results);
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
  }, [fragment, mentionOpen, searchMentions]);

  const syncMention = (text: string, caret: number) => {
    caretRef.current = caret;
    const frag = mentionFragmentBefore(text, caret);
    if (frag === null || !searchMentions) {
      setMentionOpen(false);
      setFragment("");
      return;
    }
    setFragment(frag);
    setMentionOpen(true);
  };

  const insertMention = (s: MentionSuggestion) => {
    const el = textareaRef.current;
    const caret = caretRef.current;
    const before = body.slice(0, caret);
    const after = body.slice(caret);
    // Replace the trailing `@fragment` with the canonical token.
    const replaced =
      before.replace(/@([A-Za-z0-9_-]*)$/, `@${s.displayName} `) + after;
    setBody(replaced);
    setMentionOpen(false);
    setFragment("");
    const nextCaret = before.replace(/@([A-Za-z0-9_-]*)$/, `@${s.displayName} `)
      .length;
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
