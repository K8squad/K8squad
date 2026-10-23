// test/markdown/MarkdownEditor.test.tsx — ISI-5784 WS-4: Write/Preview authoring
// affordance. Covers the pure format transform and the component (tab toggle,
// preview reuses <Markdown>, toolbar inserts, controlled mode).

import React, { useRef, useState } from "react";
import { describe, it, expect, afterEach, vi } from "vitest";
import { render, screen, cleanup, fireEvent } from "@testing-library/react";
import {
  MarkdownEditor,
  applyMarkdownFormat,
  type MarkdownEditorMode,
} from "@/components/markdown/MarkdownEditor";

afterEach(cleanup);

// ── Pure transform ──────────────────────────────────────────────────────────

describe("applyMarkdownFormat", () => {
  it("wraps a selection in bold and keeps the inner text selected", () => {
    const r = applyMarkdownFormat("say hi there", 4, 6, "bold");
    expect(r.value).toBe("say **hi** there");
    expect(r.value.slice(r.selectionStart, r.selectionEnd)).toBe("hi");
  });

  it("inserts a selected placeholder when nothing is selected (italic)", () => {
    const r = applyMarkdownFormat("", 0, 0, "italic");
    expect(r.value).toBe("_italic text_");
    expect(r.value.slice(r.selectionStart, r.selectionEnd)).toBe("italic text");
  });

  it("wraps inline code", () => {
    const r = applyMarkdownFormat("run x now", 4, 5, "code");
    expect(r.value).toBe("run `x` now");
  });

  it("builds a link and leaves the url placeholder selected", () => {
    const r = applyMarkdownFormat("see docs", 4, 8, "link");
    expect(r.value).toBe("see [docs](url)");
    expect(r.value.slice(r.selectionStart, r.selectionEnd)).toBe("url");
  });

  it("prefixes each selected line for an unordered list", () => {
    const r = applyMarkdownFormat("one\ntwo", 0, 7, "ul");
    expect(r.value).toBe("- one\n- two");
  });

  it("is idempotent on already-bulleted lines", () => {
    const r = applyMarkdownFormat("- one\ntwo", 0, 9, "ul");
    expect(r.value).toBe("- one\n- two");
  });
});

// ── Component ────────────────────────────────────────────────────────────────

function Harness({
  initial = "",
  showToolbar = true,
}: {
  initial?: string;
  showToolbar?: boolean;
}) {
  const [value, setValue] = useState(initial);
  const [mode, setMode] = useState<MarkdownEditorMode>("write");
  const ref = useRef<HTMLTextAreaElement | null>(null);
  return (
    <MarkdownEditor
      value={value}
      onChange={setValue}
      textareaRef={ref}
      mode={mode}
      onModeChange={setMode}
      showToolbar={showToolbar}
      testId="ed"
    >
      <textarea
        ref={ref}
        className="md-editor__textarea"
        data-testid="ed-input"
        value={value}
        onChange={(e) => setValue(e.target.value)}
      />
    </MarkdownEditor>
  );
}

describe("<MarkdownEditor> — Write/Preview", () => {
  it("defaults to Write mode showing the textarea, not a preview", () => {
    render(<Harness initial="# hi" />);
    expect(screen.getByTestId("ed-input")).toBeTruthy();
    expect(screen.queryByTestId("ed-preview")).toBeNull();
  });

  it("Preview tab renders the Markdown of the current value via <Markdown>", () => {
    render(<Harness initial={"# Heading\n\nbody **bold**"} />);
    fireEvent.click(screen.getByTestId("ed-tab-preview"));
    expect(screen.getByTestId("ed-preview")).toBeTruthy();
    // heading rendered through the shared renderer
    expect(screen.getByRole("heading", { name: "Heading" })).toBeTruthy();
    expect(document.querySelector(".ksq-markdown")).toBeTruthy();
    // textarea is gone while previewing
    expect(screen.queryByTestId("ed-input")).toBeNull();
  });

  it("empty value previews a friendly placeholder, not an empty render", () => {
    render(<Harness initial="" />);
    fireEvent.click(screen.getByTestId("ed-tab-preview"));
    expect(screen.getByText("Nothing to preview")).toBeTruthy();
  });

  it("toggles back to Write and restores the textarea", () => {
    render(<Harness initial="x" />);
    fireEvent.click(screen.getByTestId("ed-tab-preview"));
    fireEvent.click(screen.getByTestId("ed-tab-write"));
    expect(screen.getByTestId("ed-input")).toBeTruthy();
  });

  it("marks the active tab with aria-selected", () => {
    render(<Harness />);
    expect(
      screen.getByTestId("ed-tab-write").getAttribute("aria-selected"),
    ).toBe("true");
    fireEvent.click(screen.getByTestId("ed-tab-preview"));
    expect(
      screen.getByTestId("ed-tab-preview").getAttribute("aria-selected"),
    ).toBe("true");
  });
});

describe("<MarkdownEditor> — toolbar", () => {
  it("shows the five minimal formatting buttons in Write mode", () => {
    render(<Harness />);
    for (const f of ["bold", "italic", "code", "link", "ul"]) {
      expect(screen.getByTestId(`ed-fmt-${f}`)).toBeTruthy();
    }
  });

  it("hides the toolbar when showToolbar is false", () => {
    render(<Harness showToolbar={false} />);
    expect(screen.queryByTestId("ed-fmt-bold")).toBeNull();
  });

  it("a bold click wraps the current selection in the textarea", () => {
    render(<Harness initial="hello world" />);
    const ta = screen.getByTestId("ed-input") as HTMLTextAreaElement;
    ta.setSelectionRange(0, 5);
    fireEvent.click(screen.getByTestId("ed-fmt-bold"));
    expect((screen.getByTestId("ed-input") as HTMLTextAreaElement).value).toBe(
      "**hello** world",
    );
  });

  it("the toolbar is hidden in Preview mode", () => {
    render(<Harness initial="x" />);
    fireEvent.click(screen.getByTestId("ed-tab-preview"));
    expect(screen.queryByTestId("ed-fmt-bold")).toBeNull();
  });
});
