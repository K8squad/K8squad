"use client";

// components/search/GlobalSearch.tsx — the top-bar global search (story 8.19 / ISI-2912).
//
// A thin, accessible combobox mounted in the shell header (8.13/8.20): type a query, a debounced BFF
// read (/api/search) returns ranked work-item hits, and a listbox dropdown lets you keyboard- or
// mouse-navigate to the hit. Every read rides the ONE authz choke point (ADR-013) — this component
// owns NO authz and fabricates NO result. The server `snippet`/`title` carry ONLY <mark> highlight
// tags and are rendered via innerHTML strictly through sanitizeSnippet (escape-all, re-admit <mark>
// only), so an injected <img onerror> can never execute.
//
// Interaction: ~200ms debounce with in-flight abort (AbortController) so a fast typist never races an
// older response; blank query shows nothing (guarded, never hits the API); loading / empty / error
// each render their own honest state. Keyboard: "/" or Cmd/Ctrl+K focuses the box, Arrow keys move
// the active option, Enter navigates it, Escape closes. ARIA: role=combobox/listbox/option with
// aria-expanded + aria-activedescendant tracking the active row.

import { useCallback, useEffect, useId, useRef, useState } from "react";
import { useRouter } from "next/navigation";
import {
  fetchSearch,
  resultHref,
  sanitizeSnippet,
  stateBadge,
  SEARCH_DROPDOWN_LIMIT,
  type SearchResponse,
  type SearchResult,
} from "@/lib/search";
import "./search.css";

const DEBOUNCE_MS = 200;

type Status = "idle" | "loading" | "ok" | "error";

export interface GlobalSearchProps {
  /** Data loader (BFF GET /api/search). Injectable for tests; defaults to the real fetch. */
  search?: (q: string, limit?: number, signal?: AbortSignal) => Promise<SearchResponse>;
}

function isEditableTarget(el: EventTarget | null): boolean {
  if (!(el instanceof HTMLElement)) return false;
  const tag = el.tagName;
  return (
    tag === "INPUT" ||
    tag === "TEXTAREA" ||
    tag === "SELECT" ||
    el.isContentEditable
  );
}

export function GlobalSearch({ search = fetchSearch }: GlobalSearchProps) {
  const router = useRouter();
  const [query, setQuery] = useState("");
  const [open, setOpen] = useState(false);
  const [status, setStatus] = useState<Status>("idle");
  const [results, setResults] = useState<SearchResult[]>([]);
  const [active, setActive] = useState(-1);

  const inputRef = useRef<HTMLInputElement>(null);
  const containerRef = useRef<HTMLDivElement>(null);
  const abortRef = useRef<AbortController | null>(null);

  const listboxId = useId();
  const optionId = (i: number) => `${listboxId}-opt-${i}`;

  const runSearch = useCallback(
    async (q: string) => {
      abortRef.current?.abort();
      const controller = new AbortController();
      abortRef.current = controller;
      setStatus("loading");
      setOpen(true);
      try {
        const resp = await search(q, SEARCH_DROPDOWN_LIMIT, controller.signal);
        if (controller.signal.aborted) return;
        setResults(resp.results);
        setActive(-1);
        setStatus("ok");
      } catch (err) {
        if (controller.signal.aborted || (err as Error)?.name === "AbortError") return;
        setResults([]);
        setStatus("error");
      }
    },
    [search],
  );

  // Debounced query → search. A blank query resets to nothing (no network, closed dropdown). Any
  // keystroke cancels the pending timer AND aborts the in-flight request so responses never race.
  useEffect(() => {
    const q = query.trim();
    if (!q) {
      abortRef.current?.abort();
      setResults([]);
      setStatus("idle");
      setActive(-1);
      setOpen(false);
      return;
    }
    const timer = setTimeout(() => void runSearch(q), DEBOUNCE_MS);
    return () => clearTimeout(timer);
  }, [query, runSearch]);

  // Global focus hotkeys: Cmd/Ctrl+K always; "/" only when not already typing in a field.
  useEffect(() => {
    function onKey(e: KeyboardEvent) {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
        e.preventDefault();
        inputRef.current?.focus();
      } else if (e.key === "/" && !isEditableTarget(e.target)) {
        e.preventDefault();
        inputRef.current?.focus();
      }
    }
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);

  // Close when focus/click leaves the widget.
  useEffect(() => {
    function onDown(e: MouseEvent) {
      if (!containerRef.current?.contains(e.target as Node)) setOpen(false);
    }
    document.addEventListener("mousedown", onDown);
    return () => document.removeEventListener("mousedown", onDown);
  }, []);

  const go = useCallback(
    (r: SearchResult) => {
      setOpen(false);
      router.push(resultHref(r));
    },
    [router],
  );

  function onKeyDown(e: React.KeyboardEvent<HTMLInputElement>) {
    if (e.key === "Escape") {
      setOpen(false);
      setActive(-1);
      return;
    }
    if (e.key === "ArrowDown") {
      if (results.length === 0) return;
      e.preventDefault();
      setOpen(true);
      setActive((i) => (i + 1) % results.length);
      return;
    }
    if (e.key === "ArrowUp") {
      if (results.length === 0) return;
      e.preventDefault();
      setOpen(true);
      setActive((i) => (i <= 0 ? results.length - 1 : i - 1));
      return;
    }
    if (e.key === "Enter") {
      if (active >= 0 && active < results.length) {
        e.preventDefault();
        go(results[active]);
      }
    }
  }

  const showPanel = open && query.trim().length > 0;

  return (
    <div className="gsearch" ref={containerRef} role="search">
      <div className="gsearch__box">
        <svg
          className="gsearch__icon"
          width="16"
          height="16"
          viewBox="0 0 24 24"
          fill="none"
          stroke="currentColor"
          strokeWidth="2"
          aria-hidden="true"
        >
          <circle cx="11" cy="11" r="7" />
          <line x1="21" y1="21" x2="16.65" y2="16.65" />
        </svg>
        <input
          ref={inputRef}
          type="search"
          className="gsearch__input"
          placeholder="Search work items…"
          aria-label="Search work items"
          role="combobox"
          aria-expanded={showPanel}
          aria-controls={listboxId}
          aria-autocomplete="list"
          aria-activedescendant={
            showPanel && active >= 0 ? optionId(active) : undefined
          }
          autoComplete="off"
          spellCheck={false}
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          onKeyDown={onKeyDown}
          onFocus={() => {
            if (query.trim() && results.length > 0) setOpen(true);
          }}
          data-testid="global-search-input"
        />
      </div>

      {showPanel && (
        <div className="gsearch__panel" data-testid="global-search-panel">
          {status === "loading" ? (
            <p className="gsearch__state muted" data-testid="global-search-loading">
              Searching…
            </p>
          ) : status === "error" ? (
            <p className="gsearch__state gsearch__state--error" data-testid="global-search-error">
              Search is unavailable right now.
            </p>
          ) : results.length === 0 ? (
            <p className="gsearch__state muted" data-testid="global-search-empty">
              No matches.
            </p>
          ) : (
            <ul
              className="gsearch__list"
              id={listboxId}
              role="listbox"
              aria-label="Search results"
              data-testid="global-search-list"
            >
              {results.map((r, i) => {
                const badge = stateBadge(r.state);
                return (
                  <li
                    key={`${r.type}:${r.id}:${i}`}
                    id={optionId(i)}
                    role="option"
                    aria-selected={i === active}
                    className="gsearch__opt"
                    data-active={i === active || undefined}
                    data-testid="global-search-option"
                    onMouseEnter={() => setActive(i)}
                    onMouseDown={(e) => {
                      // mousedown (before the container's blur-close) so the click always lands.
                      e.preventDefault();
                      go(r);
                    }}
                  >
                    <a
                      href={resultHref(r)}
                      className="gsearch__optlink"
                      tabIndex={-1}
                      onClick={(e) => e.preventDefault()}
                    >
                      <span className="gsearch__optrow">
                        <span
                          className="gsearch__title"
                          // sanitizeSnippet escapes all html and re-admits ONLY <mark>.
                          dangerouslySetInnerHTML={{ __html: sanitizeSnippet(r.title) }}
                        />
                        <span className={`gsearch__badge gsearch__badge--${badge.tone}`}>
                          {badge.label}
                        </span>
                      </span>
                      {r.snippet && (
                        <span
                          className="gsearch__snippet muted"
                          dangerouslySetInnerHTML={{ __html: sanitizeSnippet(r.snippet) }}
                        />
                      )}
                    </a>
                  </li>
                );
              })}
            </ul>
          )}
        </div>
      )}
    </div>
  );
}
