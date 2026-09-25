"use client";

// MentionPopover — the suggestion list for the composer's `@`-trigger
// (ISI-4929, plan §4.3, riding the ISI-4926 mention-search endpoint). Purely
// presentational: the Composer owns the query/caret state; this renders the
// results as buttons. Agent suggestions lead (the endpoint orders agents
// first); each row shows type + display name + state chip so an agent's
// presence bucket and a work item's lane stay legible in the popover itself.

import type { MentionSuggestion } from "@/lib/discussion/types";

export interface MentionPopoverProps {
  suggestions: readonly MentionSuggestion[];
  /** Index of the keyboard-highlighted row (wraps in the Composer). */
  activeIndex: number;
  loading?: boolean;
  onSelect: (suggestion: MentionSuggestion) => void;
  onDismiss: () => void;
}

export function MentionPopover({
  suggestions,
  activeIndex,
  loading,
  onSelect,
  onDismiss,
}: MentionPopoverProps) {
  return (
    <div
      className="ksq-mention-popover"
      data-testid="mention-popover"
      role="listbox"
      aria-label="Mention suggestions"
      aria-busy={loading ? "true" : "false"}
    >
      <div className="ksq-mention-popover__head">
        <span>Mentions</span>
        <button
          type="button"
          className="ksq-mention-popover__close"
          data-testid="mention-close"
          aria-label="Close suggestions"
          onClick={onDismiss}
        >
          ×
        </button>
      </div>
      {loading ? (
        <p className="ksq-mention-popover__empty" data-testid="mention-loading">
          Searching…
        </p>
      ) : suggestions.length === 0 ? (
        <p className="ksq-mention-popover__empty" data-testid="mention-empty">
          No matches.
        </p>
      ) : (
        <ul className="ksq-mention-popover__list">
          {suggestions.map((s, i) => (
            <li key={`${s.type}:${s.id}`} role="presentation">
              <button
                type="button"
                className="ksq-mention-popover__option"
                data-testid="mention-option"
                data-mention-type={s.type}
                data-active={i === activeIndex ? "true" : "false"}
                role="option"
                aria-selected={i === activeIndex ? "true" : "false"}
                onMouseDown={(e) => e.preventDefault()} // keep textarea focus
                onClick={() => onSelect(s)}
              >
                <span className="ksq-mention-popover__glyph">
                  {s.type === "agent" ? "@" : "#"}
                </span>
                <span className="ksq-mention-popover__name">
                  {s.displayName}
                </span>
                {s.state ? (
                  <span className="ksq-mention-popover__state">{s.state}</span>
                ) : null}
              </button>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
