"use client";

// DiscussionsPanel — the collapsible left "Discussions" rail for the room
// (ISI-5673 / ISI-5598 WS-C). It surfaces the EXISTING multi-thread backend:
// one row per `discussion.thread` ("chat"), a New-chat affordance, and
// open-existing selection. Adapted from the ProjectsNavTree rail pattern
// (ISI-4090) — collapse/expand persisted to localStorage, honest
// loading/empty/error states, disclosure a11y — but self-contained
// (`ksq-dpanel__*` classes) so it owns the active-row accent treatment the mock
// calls for without depending on the nav-scoped `.rail .rail__link[data-active]`
// rules.
//
// Selection/creation are LIFTED to RoomClient (which owns the POST, the active
// thread, and the `?thread={id}` URL); this component is a controlled view. The
// "inline Untitled + rename" new-chat affordance (per the ISI-5605 mock)
// collects the chat TITLE inline and hands it up on commit — the backend
// `OpenThread` creates thread+first-message atomically and has no rename
// endpoint, so the title is captured at creation rather than patched after.

import {
  useCallback,
  useEffect,
  useLayoutEffect,
  useRef,
  useState,
} from "react";
import type { Thread } from "@/lib/discussion/types";
import { relativeActivity } from "@/lib/discussion/threadList";

export type PanelListState = "loading" | "ready" | "error";

// Mirrors ProjectsNavTree's PROJECTS_EXPAND_KEY convention (plan + ISI-5605 spec
// §1): panel-open state persisted per user. "0" collapses to the icon-rail.
export const PANEL_EXPAND_KEY = "ksquad.discussion.panel.expanded";

export interface DiscussionsPanelProps {
  threads: Thread[];
  activeThreadId: string | null;
  listState: PanelListState;
  /** Open an existing chat (RoomClient pushes `?thread={id}`). */
  onSelect: (id: string) => void;
  /** Commit a new chat with the (possibly default) title; RoomClient POSTs + selects it. */
  onNewChat: (title: string) => void;
  /** Re-run the thread-list load after an error. */
  onRetry: () => void;
  /**
   * Thread ids with a live run / unread signal → a status dot on the row
   * (decorative; the accessible name folds in the state). Optional: the
   * thread-list read exposes no per-thread live/unread signal today, so this is
   * empty in the current wiring and the dots stay off until a signal exists.
   */
  liveThreadIds?: ReadonlySet<string>;
  /** Injected clock for deterministic timestamps under test. */
  now?: number;
  /** Test/SSR override for the persisted collapse state. */
  defaultExpanded?: boolean;
}

export function DiscussionsPanel({
  threads,
  activeThreadId,
  listState,
  onSelect,
  onNewChat,
  onRetry,
  liveThreadIds,
  now,
  defaultExpanded,
}: DiscussionsPanelProps) {
  const [expanded, setExpanded] = useState<boolean>(defaultExpanded ?? true);
  // Inline new-chat editor: null = not creating; a string = the draft title.
  const [draftTitle, setDraftTitle] = useState<string | null>(null);
  const draftRef = useRef<HTMLInputElement | null>(null);

  // Restore the persisted collapse state once after mount (SSR-safe: the server
  // renders expanded, the client reconciles). An explicit defaultExpanded wins.
  useEffect(() => {
    if (defaultExpanded !== undefined) return;
    try {
      if (localStorage.getItem(PANEL_EXPAND_KEY) === "0") setExpanded(false);
    } catch {
      /* storage blocked — stay expanded, cosmetic only */
    }
  }, [defaultExpanded]);

  // Focus the inline rename field as soon as it opens (useLayoutEffect so the
  // caret is placed before paint — no flash of an unfocused field).
  useLayoutEffect(() => {
    if (draftTitle !== null) draftRef.current?.select();
  }, [draftTitle]);

  const toggle = useCallback(() => {
    setExpanded((v) => {
      const next = !v;
      try {
        localStorage.setItem(PANEL_EXPAND_KEY, next ? "1" : "0");
      } catch {
        /* ignore */
      }
      return next;
    });
  }, []);

  const beginNewChat = useCallback(() => {
    // A collapsed rail has no room for the inline field — expand first.
    setExpanded(true);
    setDraftTitle("");
  }, []);

  const commitNewChat = useCallback(() => {
    // Blank title → "Untitled" (the mock default); the thread is created with it.
    onNewChat((draftTitle ?? "").trim() || "Untitled");
    setDraftTitle(null);
  }, [draftTitle, onNewChat]);

  const nowMs = now ?? Date.now();

  return (
    <nav
      className="ksq-dpanel"
      data-collapsed={!expanded || undefined}
      data-testid="discussions-panel"
      aria-label="Discussions"
    >
      <div className="ksq-dpanel__head">
        {expanded && <h2 className="ksq-dpanel__title">Discussions</h2>}
        <button
          type="button"
          className="ksq-dpanel__toggle"
          aria-expanded={expanded}
          aria-label={expanded ? "Collapse Discussions" : "Expand Discussions"}
          data-testid="discussions-panel-toggle"
          onClick={toggle}
        >
          <span aria-hidden="true">{expanded ? "«" : "»"}</span>
        </button>
      </div>

      <button
        type="button"
        className="ksq-dpanel__new"
        data-testid="discussions-new-chat"
        onClick={beginNewChat}
        title="New chat"
      >
        <span className="ksq-dpanel__new-icon" aria-hidden="true">
          +
        </span>
        {expanded && <span className="ksq-dpanel__new-label">New chat</span>}
      </button>

      {expanded && draftTitle !== null && (
        <div className="ksq-dpanel__draft" data-testid="discussions-new-chat-draft">
          <input
            ref={draftRef}
            className="ksq-dpanel__draft-input"
            value={draftTitle}
            placeholder="Untitled"
            aria-label="New chat title"
            data-testid="discussions-new-chat-input"
            onChange={(e) => setDraftTitle(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter") {
                e.preventDefault();
                commitNewChat();
              } else if (e.key === "Escape") {
                e.preventDefault();
                setDraftTitle(null);
              }
            }}
            onBlur={() => setDraftTitle(null)}
          />
        </div>
      )}

      {listState === "loading" && (
        <p
          className="ksq-dpanel__state"
          data-testid="discussions-panel-loading"
          role="status"
        >
          <span className="ksq-dpanel__spinner" aria-hidden="true" />
          {expanded && "Loading discussions…"}
        </p>
      )}

      {listState === "error" && (
        <p className="ksq-dpanel__state" data-testid="discussions-panel-error">
          {expanded && <span>Couldn’t load discussions.</span>}
          <button type="button" className="ksq-dpanel__retry" onClick={onRetry}>
            Retry
          </button>
        </p>
      )}

      {listState === "ready" && threads.length === 0 && (
        <p
          className="ksq-dpanel__state ksq-dpanel__state--muted"
          data-testid="discussions-panel-empty"
        >
          {expanded ? "No discussions yet" : "—"}
        </p>
      )}

      {listState === "ready" && threads.length > 0 && (
        <ul className="ksq-dpanel__list" data-testid="discussions-list">
          {threads.map((t) => {
            const active = t.id === activeThreadId;
            const live = liveThreadIds?.has(t.id) ?? false;
            const ts = relativeActivity(t.createdAt, nowMs);
            // The accessible name folds in state (NavTree rail__livebadge pattern):
            // "<title>, live" when a run is active; the dot itself is decorative.
            const label = live ? `${t.title}, live` : t.title;
            return (
              <li key={t.id} className="ksq-dpanel__row">
                <a
                  href={`?thread=${encodeURIComponent(t.id)}`}
                  className="ksq-dpanel__link"
                  data-active={active || undefined}
                  aria-current={active ? "page" : undefined}
                  aria-label={label}
                  title={t.title}
                  data-testid={`discussions-row-${t.id}`}
                  onClick={(e) => {
                    // Plain click selects in-app (router.push `?thread`); let
                    // modified clicks (new tab / copy link) use the real href.
                    if (
                      e.defaultPrevented ||
                      e.button !== 0 ||
                      e.metaKey ||
                      e.ctrlKey ||
                      e.shiftKey ||
                      e.altKey
                    ) {
                      return;
                    }
                    e.preventDefault();
                    onSelect(t.id);
                  }}
                >
                  {live && (
                    <span
                      className="ksq-dpanel__dot"
                      data-kind="live"
                      aria-hidden="true"
                    />
                  )}
                  {expanded ? (
                    <>
                      <span className="ksq-dpanel__rowtitle">{t.title}</span>
                      {ts && (
                        <time className="ksq-dpanel__rowts" dateTime={t.createdAt}>
                          {ts}
                        </time>
                      )}
                    </>
                  ) : (
                    <span className="ksq-dpanel__tile" aria-hidden="true">
                      {(t.title.trim()[0] ?? "•").toUpperCase()}
                    </span>
                  )}
                </a>
              </li>
            );
          })}
        </ul>
      )}
    </nav>
  );
}
