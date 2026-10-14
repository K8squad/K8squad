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
import {
  matchSlashCommands,
  resolveSlashCommand,
  slashFragment,
  type SlashCommand,
} from "@/lib/discussion/slash";
import {
  MarkdownEditor,
  type MarkdownEditorMode,
} from "@/components/markdown/MarkdownEditor";
import { MentionPopover } from "./MentionPopover";
import { SlashCommandMenu } from "./SlashCommandMenu";

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
  /**
   * Slash-command handler (ISI-5674 WS-D). When wired, a leading `/` opens the
   * command menu and a resolved command (e.g. `/reset`) is INTERCEPTED — passed
   * here by name and NEVER posted as a literal message. Absent ⇒ no command menu
   * and `/`-prefixed text posts verbatim (the pre-WS-D behaviour).
   */
  onCommand?: (name: string) => void;
}

const MENTION_DEBOUNCE_MS = 200;

export function Composer({
  parentId,
  onPost,
  placeholder,
  directTargets,
  searchMentions,
  onCommand,
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
  // Slash-command menu (ISI-5674 WS-D): open while the body is a leading-`/`
  // command context; `commandIndex` is the keyboard-highlighted row. Only when
  // `onCommand` is wired — otherwise `/` text is ordinary and posts verbatim.
  const [commandOpen, setCommandOpen] = useState(false);
  const [commandIndex, setCommandIndex] = useState(0);
  // Write/Preview authoring mode (ISI-5784 WS-4). Lifted here so the slash and
  // mention popovers render only in Write mode (Preview hides the textarea).
  const [composeMode, setComposeMode] = useState<MarkdownEditorMode>("write");
  const commands: SlashCommand[] = commandOpen
    ? matchSlashCommands(slashFragment(body) ?? "")
    : [];
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

  // Intercept a resolved slash command (ISI-5674 WS-D): hand the name to the
  // parent and clear the box — the command text is NEVER posted. Closes any open
  // menu/popover and drops collected ticket refs (a `/reset` has none).
  const runCommand = (cmd: SlashCommand) => {
    if (!onCommand) return;
    onCommand(cmd.name);
    setBody("");
    setRefs([]);
    setCommandOpen(false);
    setMentionOpen(false);
    setFragment("");
    textareaRef.current?.focus();
  };

  // Route a body change to the command menu (leading `/`) or the mention popover
  // (`@`/`#`). The two are mutually exclusive: a command context is a LEADING
  // slash with no space, so no `@`/`#` fragment can also be live.
  const onBodyChange = (text: string, caret: number) => {
    setBody(text);
    if (onCommand && slashFragment(text) !== null) {
      setCommandOpen(true);
      setCommandIndex(0);
      // A command context can't also be a mention fragment — close that popover.
      setMentionOpen(false);
      setFragment("");
      return;
    }
    setCommandOpen(false);
    syncMention(text, caret);
  };

  const onBodyKeyDown = (e: React.KeyboardEvent<HTMLTextAreaElement>) => {
    // Command menu takes keyboard priority while open (ISI-5674 WS-D).
    if (commandOpen) {
      if (e.key === "Escape") {
        e.preventDefault();
        setCommandOpen(false);
        return;
      }
      if (commands.length === 0) return;
      if (e.key === "ArrowDown") {
        e.preventDefault();
        setCommandIndex((i) => (i + 1) % commands.length);
        return;
      }
      if (e.key === "ArrowUp") {
        e.preventDefault();
        setCommandIndex((i) => (i - 1 + commands.length) % commands.length);
        return;
      }
      if (e.key === "Enter" || e.key === "Tab") {
        e.preventDefault();
        runCommand(commands[commandIndex]);
        return;
      }
      return;
    }
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
    // Intercept a bare slash command (e.g. `/reset`) BEFORE any post — it runs
    // the command, never posts the literal text (ISI-5674 WS-D, plan §WS-D.1).
    if (onCommand) {
      const cmd = resolveSlashCommand(body);
      if (cmd) {
        runCommand(cmd);
        return;
      }
    }
    if (!canSubmit(input)) return;
    await onPost(buildPostBody(input));
    setBody("");
    setRefs([]);
    setMentionOpen(false);
    setCommandOpen(false);
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
        <MarkdownEditor
          className="ksq-composer__editor"
          value={body}
          onChange={setBody}
          textareaRef={textareaRef}
          mode={composeMode}
          onModeChange={setComposeMode}
          testId="composer-editor"
        >
          <textarea
            ref={textareaRef}
            className="ksq-composer__body md-editor__textarea"
            data-testid="composer-body"
            aria-label={parentId ? "Reply in thread" : "Post a message"}
            placeholder={
              placeholder ?? (parentId ? "Reply…" : "Post to the room…")
            }
            value={body}
            onChange={(e) =>
              onBodyChange(e.target.value, e.target.selectionStart ?? 0)
            }
            onKeyDown={onBodyKeyDown}
          />
        </MarkdownEditor>
        <button
          type="submit"
          className="ksq-composer__submit"
          data-testid="composer-submit"
          disabled={disabled}
        >
          {parentId ? "Reply" : "Post"}
        </button>
      </div>
      {composeMode === "write" && commandOpen ? (
        <SlashCommandMenu
          commands={commands}
          activeIndex={commandIndex}
          onSelect={runCommand}
          onDismiss={() => setCommandOpen(false)}
        />
      ) : null}
      {composeMode === "write" && mentionOpen ? (
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
