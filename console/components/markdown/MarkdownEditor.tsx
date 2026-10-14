"use client";

// MarkdownEditor — the WS-4 authoring affordance (ISI-5784, plan ISI-5776).
//
// A thin Write/Preview tab strip + optional minimal formatting toolbar that
// WRAPS an existing <textarea> — it does NOT replace it. The host composer keeps
// owning the textarea (and its @-mention popover, slash-command menu, caret
// tracking, keyboard handling); this component only adds the tab chrome and a
// Preview tab that reuses the shared hardened <Markdown> renderer (WS-1).
//
// No new editor library, no contenteditable, no WYSIWYG — the "no rich-text
// black box" principle from the WS-5 design spec (hack/isi-5781-markdown-design-spec.md §8).
//
// Mode is CONTROLLED when `mode`/`onModeChange` are supplied (the composers do
// this so they can hide their popovers in Preview mode); otherwise it is local.

import React, { useState } from "react";

import { Markdown } from "./Markdown";

import "./markdown-editor.css";

export type MarkdownEditorMode = "write" | "preview";

/** The minimal toolbar actions (WS-5 spec §8.5). No heading button by design. */
export type MarkdownFormat = "bold" | "italic" | "code" | "link" | "ul";

export interface FormatResult {
  value: string;
  /** Caret/selection to restore after the edit. */
  selectionStart: number;
  selectionEnd: number;
}

interface WrapSpec {
  prefix: string;
  suffix: string;
  placeholder: string;
}

const WRAP: Record<"bold" | "italic" | "code", WrapSpec> = {
  bold: { prefix: "**", suffix: "**", placeholder: "bold text" },
  italic: { prefix: "_", suffix: "_", placeholder: "italic text" },
  code: { prefix: "`", suffix: "`", placeholder: "code" },
};

/**
 * Pure selection transform — wraps/prefixes the current selection with Markdown
 * syntax and returns the new value plus the selection to restore. Exported so it
 * can be unit-tested without a DOM.
 *
 * - bold/italic/code: wrap the selection (or insert a selected placeholder).
 * - link: `[text](url)` with the `url` placeholder left selected for quick typing.
 * - ul: prefix each line of the selection with `- ` (idempotent per line).
 */
export function applyMarkdownFormat(
  value: string,
  selStart: number,
  selEnd: number,
  fmt: MarkdownFormat,
): FormatResult {
  const selected = value.slice(selStart, selEnd);
  const before = value.slice(0, selStart);
  const after = value.slice(selEnd);

  if (fmt === "bold" || fmt === "italic" || fmt === "code") {
    const { prefix, suffix, placeholder } = WRAP[fmt];
    const inner = selected || placeholder;
    const next = before + prefix + inner + suffix + after;
    const innerStart = before.length + prefix.length;
    return {
      value: next,
      selectionStart: innerStart,
      selectionEnd: innerStart + inner.length,
    };
  }

  if (fmt === "link") {
    const text = selected || "text";
    const url = "url";
    const next = `${before}[${text}](${url})${after}`;
    // Leave the `url` placeholder selected: "[" + text + "](" = 3 + text.length
    const urlStart = before.length + 1 + text.length + 2;
    return {
      value: next,
      selectionStart: urlStart,
      selectionEnd: urlStart + url.length,
    };
  }

  // Unordered list: expand to whole lines, prefix each with "- " (skip lines
  // already bulleted so re-clicking is a no-op).
  const lineStart = before.lastIndexOf("\n") + 1;
  const head = value.slice(0, lineStart);
  const block = value.slice(lineStart, selEnd);
  const lines = block === "" ? [""] : block.split("\n");
  const prefixed = lines
    .map((l) => (l.startsWith("- ") ? l : `- ${l}`))
    .join("\n");
  const next = head + prefixed + after;
  return {
    value: next,
    selectionStart: head.length,
    selectionEnd: head.length + prefixed.length,
  };
}

export interface MarkdownEditorProps {
  /** Current text — the host composer's body state. */
  value: string;
  /** Applied only by the toolbar's formatting inserts. */
  onChange: (next: string) => void;
  /** Ref to the host textarea rendered as `children` (for selection ops). */
  textareaRef: React.RefObject<HTMLTextAreaElement | null>;
  /** The Write-mode input — the host textarea. Rendered only in Write mode. */
  children: React.ReactNode;
  /** Controlled mode; omit for self-managed local mode. */
  mode?: MarkdownEditorMode;
  onModeChange?: (m: MarkdownEditorMode) => void;
  /** Initial mode when uncontrolled (default "write"). */
  defaultMode?: MarkdownEditorMode;
  /** Show the minimal formatting toolbar (default true). */
  showToolbar?: boolean;
  /** Disable the toolbar (e.g. while posting). */
  disabled?: boolean;
  /** Extra class on the editor shell (e.g. flex sizing in the host row). */
  className?: string;
  /** Base for data-testids: `{testId}`, `-tab-write`, `-tab-preview`, `-preview`. */
  testId?: string;
}

interface ToolbarButton {
  fmt: MarkdownFormat;
  label: string;
  content: React.ReactNode;
}

const TOOLBAR: ToolbarButton[] = [
  { fmt: "bold", label: "Bold", content: <strong>B</strong> },
  { fmt: "italic", label: "Italic", content: <em>I</em> },
  { fmt: "code", label: "Inline code", content: <code>{"</>"}</code> },
  {
    fmt: "link",
    label: "Link",
    content: (
      <svg
        width="14"
        height="14"
        viewBox="0 0 24 24"
        fill="none"
        stroke="currentColor"
        strokeWidth="2"
        strokeLinecap="round"
        strokeLinejoin="round"
        aria-hidden="true"
      >
        <path d="M10 13a5 5 0 0 0 7.54.54l3-3a5 5 0 0 0-7.07-7.07l-1.72 1.71" />
        <path d="M14 11a5 5 0 0 0-7.54-.54l-3 3a5 5 0 0 0 7.07 7.07l1.71-1.71" />
      </svg>
    ),
  },
  { fmt: "ul", label: "Bulleted list", content: <span aria-hidden="true">•</span> },
];

export function MarkdownEditor({
  value,
  onChange,
  textareaRef,
  children,
  mode,
  onModeChange,
  defaultMode = "write",
  showToolbar = true,
  disabled = false,
  className,
  testId = "md-editor",
}: MarkdownEditorProps) {
  const [localMode, setLocalMode] = useState<MarkdownEditorMode>(defaultMode);
  const current = mode ?? localMode;
  const setMode = (m: MarkdownEditorMode) => {
    if (onModeChange) onModeChange(m);
    if (mode === undefined) setLocalMode(m);
  };

  const runFormat = (fmt: MarkdownFormat) => {
    const el = textareaRef.current;
    const start = el?.selectionStart ?? value.length;
    const end = el?.selectionEnd ?? value.length;
    const res = applyMarkdownFormat(value, start, end, fmt);
    onChange(res.value);
    if (el) {
      el.focus();
      requestAnimationFrame(() =>
        el.setSelectionRange(res.selectionStart, res.selectionEnd),
      );
    }
  };

  return (
    <div
      className={`md-editor${className ? ` ${className}` : ""}`}
      data-testid={testId}
    >
      <div className="md-editor__tabs" role="tablist" aria-label="Compose mode">
        <button
          type="button"
          role="tab"
          aria-selected={current === "write"}
          className={`md-editor__tab${current === "write" ? " md-editor__tab--active" : ""}`}
          data-testid={`${testId}-tab-write`}
          onClick={() => setMode("write")}
        >
          Write
        </button>
        <button
          type="button"
          role="tab"
          aria-selected={current === "preview"}
          className={`md-editor__tab${current === "preview" ? " md-editor__tab--active" : ""}`}
          data-testid={`${testId}-tab-preview`}
          onClick={() => setMode("preview")}
        >
          Preview
        </button>
      </div>

      {current === "write" && showToolbar ? (
        <div
          className="md-editor__toolbar"
          role="toolbar"
          aria-label="Formatting"
        >
          {TOOLBAR.map((b) => (
            <button
              key={b.fmt}
              type="button"
              className="md-editor__toolbar-btn"
              data-testid={`${testId}-fmt-${b.fmt}`}
              aria-label={b.label}
              title={b.label}
              disabled={disabled}
              // Keep the textarea selection intact across the click.
              onMouseDown={(e) => e.preventDefault()}
              onClick={() => runFormat(b.fmt)}
            >
              {b.content}
            </button>
          ))}
        </div>
      ) : null}

      {current === "write" ? (
        children
      ) : (
        <div className="md-editor__preview" data-testid={`${testId}-preview`}>
          {value.trim() ? (
            <Markdown>{value}</Markdown>
          ) : (
            <p className="md-editor__preview-empty">Nothing to preview</p>
          )}
        </div>
      )}
    </div>
  );
}
