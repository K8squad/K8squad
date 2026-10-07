"use client";

// components/inbox/InboxList.tsx — ISI-5535 (E1) + ISI-5537 (E3) of ISI-5531: the "Needs Human
// Decision" inbox. E1 shipped the read-only list (poll, chip, live marker, unread dot, click-through);
// E3 adds the TRIAGE layer, mock ux/isi-5531-inbox/04-inbox-triage:
//   • inline answer WITHOUT leaving the list — approve → Approve/Reject; choose_one → numbered
//     option buttons (1–9) + "Something else"; choose_many/free_form → "Open to answer" (detail).
//   • full keyboard driving — J/K move · Enter open · 1–9 pick option · A/R approve/reject · U
//     read/unread. (H snooze is E4/backlog, not bound here.)
//   • read/unread toggle (U), on top of E1's mark-read-on-open.
//   • filters — Mine (admin fleet→team), decision-type, project.
//
// Decision chip hues: review→violet, proposal/approve→green, choose_one/choose_many→blue,
// free_form→amber. Inline answer/reject POST to the project-scoped decision endpoints via the shared
// discussion client (ADR-0026 §4.4), keyed by item.decision.messageId.

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useRouter } from "next/navigation";
import {
  decisionChipHue,
  fetchInbox,
  inboxItemHref,
  inboxMarkSeen,
  inboxMarkUnread,
  type InboxItem,
  type InboxResponse,
} from "@/lib/inbox/api";
import {
  createDiscussionClient,
  type DiscussionClient,
} from "@/lib/discussion/api";
import "./inbox.css";

const POLL_MS = 5_000;

function relativeTime(iso: string): string {
  const diff = Math.floor((Date.now() - new Date(iso).getTime()) / 1000);
  if (diff < 60) return `${diff}s ago`;
  if (diff < 3600) return `${Math.floor(diff / 60)}m ago`;
  if (diff < 86400) return `${Math.floor(diff / 3600)}h ago`;
  return `${Math.floor(diff / 86400)}d ago`;
}

const DECISION_LABELS: Record<string, string> = {
  review: "Review",
  proposal: "Proposal",
  approve: "Approve",
  choose_one: "Choose option",
  choose_many: "Choose options",
  free_form: "Question",
};

function decisionLabel(type: string): string {
  return DECISION_LABELS[type] ?? type;
}

function DecisionChip({ type }: { type: string }) {
  const hue = decisionChipHue(type);
  const label = decisionLabel(type);
  return (
    <span className={`inbox__chip inbox__chip--${hue}`} aria-label={`Decision type: ${label}`}>
      {label}
    </span>
  );
}

// ---------------------------------------------------------------------------
// Row
// ---------------------------------------------------------------------------

interface RowHandlers {
  onOpen: (item: InboxItem) => void;
  onApprove: (item: InboxItem) => void;
  onReject: (item: InboxItem) => void;
  onChoose: (item: InboxItem, optionId: string) => void;
  onToggleRead: (item: InboxItem) => void;
}

/** inline answer is offered in the row only for approve + choose_one (mock 04). */
function offersInlineAnswer(item: InboxItem): boolean {
  return item.decision != null && (item.decisionType === "approve" || item.decisionType === "choose_one");
}

function InboxRow({
  item,
  focused,
  busy,
  error,
  rowRef,
  handlers,
}: {
  item: InboxItem;
  focused: boolean;
  busy: boolean;
  error?: string;
  rowRef: (el: HTMLLIElement | null) => void;
  handlers: RowHandlers;
}) {
  const timeLabel = item.lastRunAt ? relativeTime(item.lastRunAt) : "";
  const d = item.decision;
  const inline = offersInlineAnswer(item);
  const openForDetail = d != null && !inline; // choose_many / free_form → open the full card

  return (
    <li
      ref={rowRef}
      role="listitem"
      tabIndex={focused ? 0 : -1}
      aria-current={focused ? "true" : undefined}
      className={
        "inbox__row" +
        (item.unread ? " inbox__row--unread" : "") +
        (focused ? " inbox__row--focused" : "")
      }
    >
      <div className="inbox__row-head">
        <button
          type="button"
          className="inbox__row-open"
          onClick={() => handlers.onOpen(item)}
        >
          <span className="inbox__row-title">
            {item.unread && <span className="inbox__unread-dot" aria-label="Unread" />}
            {item.title}
            {item.live && (
              <span className="inbox__live-badge rail__livebadge" aria-label="Live run" />
            )}
          </span>
          <span className="inbox__row-meta">
            <DecisionChip type={item.decisionType} />
            {item.raisedByAgent && <span className="inbox__raised-by">· {item.raisedByAgent}</span>}
            {timeLabel && <span className="inbox__time">{timeLabel}</span>}
          </span>
        </button>
        {focused && (
          <span className="inbox__kbd-hint" aria-hidden="true">
            <kbd>J</kbd>
            <kbd>K</kbd>
          </span>
        )}
      </div>

      {/* Inline answer controls (ISI-5537 E3) */}
      {inline && item.decisionType === "approve" && (
        <div className="inbox__inline" data-testid="inline-approve">
          <button
            type="button"
            className="inbox__btn inbox__btn--approve"
            data-testid="inline-approve-btn"
            disabled={busy}
            onClick={() => handlers.onApprove(item)}
          >
            Approve
          </button>
          {d?.allowReject && (
            <button
              type="button"
              className="inbox__btn inbox__btn--reject"
              data-testid="inline-reject-btn"
              disabled={busy}
              onClick={() => handlers.onReject(item)}
            >
              Reject
            </button>
          )}
          <span className="inbox__inline-hint" aria-hidden="true">
            <kbd>A</kbd>
            {d?.allowReject && <kbd>R</kbd>}
          </span>
        </div>
      )}

      {inline && item.decisionType === "choose_one" && (
        <div className="inbox__inline" data-testid="inline-choose">
          {(d?.options ?? []).slice(0, 9).map((o, i) => (
            <button
              key={o.id}
              type="button"
              className={
                "inbox__btn inbox__opt" + (o.recommended ? " inbox__opt--recommended" : "")
              }
              data-testid="inline-option"
              disabled={busy}
              onClick={() => handlers.onChoose(item, o.id)}
            >
              <span className="inbox__opt-key" aria-hidden="true">
                {i + 1}
              </span>
              {o.label}
            </button>
          ))}
          <button
            type="button"
            className="inbox__btn inbox__opt inbox__opt--else"
            data-testid="inline-something-else"
            disabled={busy}
            onClick={() => handlers.onOpen(item)}
          >
            Something else
          </button>
          <span className="inbox__inline-hint" aria-hidden="true">
            <kbd>1–9</kbd>
          </span>
        </div>
      )}

      {openForDetail && (
        <div className="inbox__inline">
          <button
            type="button"
            className="inbox__open-link"
            data-testid="inline-open-detail"
            onClick={() => handlers.onOpen(item)}
          >
            Open to answer <span aria-hidden="true">›</span>
          </button>
          <span className="inbox__inline-hint" aria-hidden="true">
            <kbd>Enter</kbd>
          </span>
        </div>
      )}

      {error && (
        <p className="inbox__row-error" role="alert" data-testid="inline-error">
          {error}
        </p>
      )}
    </li>
  );
}

// ---------------------------------------------------------------------------
// List container
// ---------------------------------------------------------------------------

export interface InboxListProps {
  /** Test seam: inject a stub discussion client for inline answer/reject. */
  client?: DiscussionClient;
}

export function InboxList({ client }: InboxListProps = {}) {
  const router = useRouter();
  const [api] = useState<DiscussionClient>(() => client ?? createDiscussionClient());

  const [data, setData] = useState<InboxResponse | null>(null);
  const [error, setError] = useState(false);
  const [mine, setMine] = useState(false);
  const [typeFilter, setTypeFilter] = useState<string>("all");
  const [projectFilter, setProjectFilter] = useState<string>("all");

  const [focusedIndex, setFocusedIndex] = useState(0);
  const [busyKey, setBusyKey] = useState<string | undefined>();
  const [rowErrors, setRowErrors] = useState<Record<string, string>>({});
  // Keys answered/rejected inline this session — filtered out immediately so the row leaves the list
  // without waiting for the next 5s poll (the backend drops them from the open arm on resolve anyway).
  const [resolvedKeys, setResolvedKeys] = useState<Set<string>>(() => new Set());
  // Optimistic read/unread overrides from the U toggle; persisted to the backend, so a later poll
  // re-derives the same value — the override just removes the 5s lag.
  const [unreadOverride, setUnreadOverride] = useState<Record<string, boolean>>({});

  // Mark-seen fires ONCE per mount (ADR-0026 §6) — opening the Inbox clears the unread dots + nav
  // badge for whatever is unread at that moment; a run that re-surfaces an item while the page is
  // open stays unread until the next visit. Resets when the scope (mine) changes → a fresh view.
  const markedSeen = useRef(false);
  const rowRefs = useRef<Array<HTMLLIElement | null>>([]);
  // True once the user starts keyboard-driving — so focus follows J/K but is never stolen on mount
  // or on a background poll (the roving tabindex still lets Tab land on the focused row first).
  const navActive = useRef(false);

  useEffect(() => {
    markedSeen.current = false;
    let cancelled = false;
    const poll = () => {
      if (document.hidden) return;
      fetchInbox({ scope: mine ? "mine" : undefined })
        .then((d) => {
          if (cancelled) return;
          setData(d);
          setError(false);
          if (!markedSeen.current) {
            markedSeen.current = true;
            const unreadKeys = d.items.filter((i) => i.unread).map((i) => i.key);
            void inboxMarkSeen(unreadKeys);
          }
        })
        .catch(() => {
          if (!cancelled) setError(true);
        });
    };
    poll();
    const id = setInterval(poll, POLL_MS);
    return () => {
      cancelled = true;
      clearInterval(id);
    };
  }, [mine]);

  // Apply session overrides (resolved rows removed, unread toggles) + the client-side filters.
  const items = useMemo(() => {
    const raw = data?.items ?? [];
    return raw
      .filter((it) => !resolvedKeys.has(it.key))
      .filter((it) => typeFilter === "all" || it.decisionType === typeFilter)
      .filter((it) => projectFilter === "all" || it.projectId === projectFilter)
      .map((it) =>
        it.key in unreadOverride ? { ...it, unread: unreadOverride[it.key] } : it,
      );
  }, [data, resolvedKeys, typeFilter, projectFilter, unreadOverride]);

  // Distinct type/project values present, for the filter dropdowns (derived from the full set so a
  // filter never hides its own option).
  const { typeOptions, projectOptions } = useMemo(() => {
    const raw = (data?.items ?? []).filter((it) => !resolvedKeys.has(it.key));
    const types = new Set<string>();
    const projects = new Set<string>();
    for (const it of raw) {
      types.add(it.decisionType);
      if (it.projectId) projects.add(it.projectId);
    }
    return { typeOptions: [...types].sort(), projectOptions: [...projects].sort() };
  }, [data, resolvedKeys]);

  // Keep focusedIndex in range as the filtered list changes.
  useEffect(() => {
    setFocusedIndex((i) => Math.max(0, Math.min(i, Math.max(0, items.length - 1))));
  }, [items.length]);

  const setRowError = useCallback((key: string, msg: string) => {
    setRowErrors((cur) => ({ ...cur, [key]: msg }));
  }, []);
  const clearRowError = useCallback((key: string) => {
    setRowErrors((cur) => {
      if (!(key in cur)) return cur;
      const next = { ...cur };
      delete next[key];
      return next;
    });
  }, []);

  const open = useCallback(
    (item: InboxItem) => {
      router.push(inboxItemHref(item));
    },
    [router],
  );

  // Answer / reject (ADR-0026 §4.4). On success the card leaves the open arm → drop it locally so the
  // row disappears immediately; on failure surface a note and keep the row (never a silent no-op).
  const submitAnswer = useCallback(
    async (item: InboxItem, body: { selectedOptionIds?: string[]; freeText?: string }) => {
      if (!item.decision || !item.projectId) return;
      setBusyKey(item.key);
      clearRowError(item.key);
      try {
        await api.answerDecisionRequest(item.projectId, item.decision.messageId, body);
        setResolvedKeys((cur) => new Set(cur).add(item.key));
      } catch (e) {
        setRowError(item.key, answerErrorMessage(e));
      } finally {
        setBusyKey(undefined);
      }
    },
    [api, clearRowError, setRowError],
  );

  const submitReject = useCallback(
    async (item: InboxItem) => {
      if (!item.decision || !item.projectId) return;
      // A reject that requires a reason can't be done inline — route to the full card.
      if (item.decision.rejectRequiresReason) {
        open(item);
        return;
      }
      setBusyKey(item.key);
      clearRowError(item.key);
      try {
        await api.rejectDecisionRequest(item.projectId, item.decision.messageId);
        setResolvedKeys((cur) => new Set(cur).add(item.key));
      } catch (e) {
        setRowError(item.key, answerErrorMessage(e));
      } finally {
        setBusyKey(undefined);
      }
    },
    [api, clearRowError, setRowError, open],
  );

  const approve = useCallback((item: InboxItem) => void submitAnswer(item, {}), [submitAnswer]);
  const choose = useCallback(
    (item: InboxItem, optionId: string) => void submitAnswer(item, { selectedOptionIds: [optionId] }),
    [submitAnswer],
  );

  const toggleRead = useCallback((item: InboxItem) => {
    const nextUnread = !item.unread;
    setUnreadOverride((cur) => ({ ...cur, [item.key]: nextUnread }));
    if (nextUnread) void inboxMarkUnread([item.key]);
    else void inboxMarkSeen([item.key]);
  }, []);

  const handlers: RowHandlers = useMemo(
    () => ({
      onOpen: open,
      onApprove: approve,
      onReject: submitReject,
      onChoose: choose,
      onToggleRead: toggleRead,
    }),
    [open, approve, submitReject, choose, toggleRead],
  );

  // Keyboard driving (mock 04). Bound on the list; only active while a row is focused.
  const onKeyDown = useCallback(
    (e: React.KeyboardEvent) => {
      if (items.length === 0) return;
      navActive.current = true;
      const idx = Math.min(focusedIndex, items.length - 1);
      const item = items[idx];
      const key = e.key;
      if (key === "j" || key === "J" || key === "ArrowDown") {
        setFocusedIndex((i) => Math.min(items.length - 1, i + 1));
        e.preventDefault();
      } else if (key === "k" || key === "K" || key === "ArrowUp") {
        setFocusedIndex((i) => Math.max(0, i - 1));
        e.preventDefault();
      } else if (key === "Enter") {
        open(item);
        e.preventDefault();
      } else if (key === "u" || key === "U") {
        toggleRead(item);
        e.preventDefault();
      } else if ((key === "a" || key === "A") && item.decisionType === "approve" && offersInlineAnswer(item)) {
        approve(item);
        e.preventDefault();
      } else if ((key === "r" || key === "R") && item.decisionType === "approve" && item.decision?.allowReject) {
        submitReject(item);
        e.preventDefault();
      } else if (key >= "1" && key <= "9" && item.decisionType === "choose_one" && item.decision) {
        const n = Number(key);
        const opt = (item.decision.options ?? [])[n - 1];
        if (opt) {
          choose(item, opt.id);
          e.preventDefault();
        }
      }
    },
    [items, focusedIndex, open, toggleRead, approve, submitReject, choose],
  );

  // Move DOM focus to the focused row so keyboard driving + screen-reader focus track together — but
  // only once the user is navigating (navActive), never stealing focus on mount or a background poll.
  useEffect(() => {
    if (!navActive.current || items.length === 0) return;
    rowRefs.current[Math.min(focusedIndex, items.length - 1)]?.focus();
  }, [focusedIndex, items.length]);

  const showMineToggle = mine || Boolean(data?.fleet);

  if (error && !data) {
    return <p className="inbox__error">Could not load inbox — retrying…</p>;
  }
  if (!data) {
    return <p className="inbox__loading">Loading…</p>;
  }

  return (
    <div className="inbox__body">
      <div className="inbox__filters" role="group" aria-label="Inbox filters">
        {showMineToggle && (
          <button
            type="button"
            className={"inbox__filter-chip" + (mine ? " inbox__filter-chip--on" : "")}
            aria-pressed={mine}
            data-testid="filter-mine"
            onClick={() => setMine((m) => !m)}
          >
            Mine
          </button>
        )}
        <label className="inbox__filter-field">
          <span className="inbox__filter-label">Type</span>
          <select
            className="inbox__filter-select"
            data-testid="filter-type"
            value={typeFilter}
            onChange={(e) => setTypeFilter(e.target.value)}
          >
            <option value="all">All types</option>
            {typeOptions.map((t) => (
              <option key={t} value={t}>
                {decisionLabel(t)}
              </option>
            ))}
          </select>
        </label>
        <label className="inbox__filter-field">
          <span className="inbox__filter-label">Project</span>
          <select
            className="inbox__filter-select"
            data-testid="filter-project"
            value={projectFilter}
            onChange={(e) => setProjectFilter(e.target.value)}
          >
            <option value="all">All projects</option>
            {projectOptions.map((p) => (
              <option key={p} value={p}>
                {p}
              </option>
            ))}
          </select>
        </label>
      </div>

      {items.length === 0 ? (
        <div className="inbox__empty">
          <p>All caught up — nothing needs a decision right now.</p>
        </div>
      ) : (
        <ul className="inbox__list" role="list" onKeyDown={onKeyDown}>
          {items.map((item, i) => (
            <InboxRow
              key={item.key}
              item={item}
              focused={i === Math.min(focusedIndex, items.length - 1)}
              busy={busyKey === item.key}
              error={rowErrors[item.key]}
              rowRef={(el) => {
                rowRefs.current[i] = el;
              }}
              handlers={handlers}
            />
          ))}
        </ul>
      )}

      {data.fleet && !mine && (
        <p className="inbox__scope-note">Showing decisions across all teams (admin). Use “Mine” to narrow.</p>
      )}
    </div>
  );
}

/** Map an answer/reject failure to a short, non-leaking note (DiscussionApiError carries .status). */
function answerErrorMessage(e: unknown): string {
  const status = (e as { status?: number } | null)?.status;
  switch (status) {
    case 409:
      return "Already answered — refreshing.";
    case 400:
      return "That answer wasn't accepted. Open to try again.";
    case 401:
    case 403:
    case 404:
      return "You can no longer act on this decision.";
    default:
      return "Couldn't submit — please try again.";
  }
}
