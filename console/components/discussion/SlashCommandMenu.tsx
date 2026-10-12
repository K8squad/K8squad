"use client";

// SlashCommandMenu — the command-menu popover for the composer's leading-`/`
// trigger (ISI-5674 WS-D, mocks ISI-5605 §3). Purely presentational: the
// Composer owns the body/fragment state and keyboard navigation; this renders
// the matched commands as buttons. Because the dock is sticky at the bottom of
// the room (ISI-5597), it opens UPWARD — the visual placement is handled in
// discussion.css (`.ksq-slash-popover` anchored above the composer row), the same
// upward treatment the mention popover already uses.
//
// Mirrors MentionPopover's listbox/option a11y shape so the two composer popovers
// read identically to AT: `role="listbox"` + `role="option"` rows with
// `aria-selected` tracking the keyboard-highlighted entry.

import type { SlashCommand } from "@/lib/discussion/slash";

export interface SlashCommandMenuProps {
  commands: readonly SlashCommand[];
  /** Index of the keyboard-highlighted row (wraps in the Composer). */
  activeIndex: number;
  onSelect: (command: SlashCommand) => void;
  onDismiss: () => void;
}

export function SlashCommandMenu({
  commands,
  activeIndex,
  onSelect,
  onDismiss,
}: SlashCommandMenuProps) {
  return (
    <div
      className="ksq-slash-popover"
      data-testid="slash-popover"
      role="listbox"
      aria-label="Commands"
    >
      <div className="ksq-slash-popover__head">
        <span>Commands</span>
        <button
          type="button"
          className="ksq-slash-popover__close"
          data-testid="slash-close"
          aria-label="Close commands"
          onClick={onDismiss}
        >
          ×
        </button>
      </div>
      {commands.length === 0 ? (
        <p className="ksq-slash-popover__empty" data-testid="slash-empty">
          No matching command.
        </p>
      ) : (
        <ul className="ksq-slash-popover__list">
          {commands.map((c, i) => (
            <li key={c.name} role="presentation">
              <button
                type="button"
                className="ksq-slash-popover__option"
                data-testid="slash-option"
                data-command={c.name}
                data-active={i === activeIndex ? "true" : "false"}
                role="option"
                aria-selected={i === activeIndex ? "true" : "false"}
                onMouseDown={(e) => e.preventDefault()} // keep textarea focus
                onClick={() => onSelect(c)}
              >
                <span className="ksq-slash-popover__glyph" aria-hidden="true">
                  /
                </span>
                <span className="ksq-slash-popover__text">
                  <span className="ksq-slash-popover__title">{c.title}</span>
                  <span className="ksq-slash-popover__subtitle">
                    {c.subtitle}
                  </span>
                </span>
              </button>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
