// test/markdown/Markdown.fidelity.test.tsx — ISI-7197 (WS-5): markdown rendering
// fidelity of agent messages. Known-good-sample audit of the shared <Markdown>
// renderer that BOTH surfaces (discussion MessageItem and the issue-thread
// comment/ProgressBody path) funnel through. What this suite pins down:
//   - a well-formed summary renders cleanly (headings, code fences, nested
//     lists, hard breaks) with no stray entities or broken fences;
//   - a SINGLE-escaped entity in prose (e.g. &#x20;) is decoded by the CommonMark
//     parser — the renderer never shows it literally;
//   - DOUBLE-escaped entities (&amp;#x20;) and entities inside code spans/fences
//     DO render literally — spec behavior, so those artifacts are producer-side,
//     not renderer bugs.

import { describe, it, expect, afterEach } from "vitest";
import { render, cleanup } from "@testing-library/react";
import { Markdown } from "@/components/markdown/Markdown";

afterEach(cleanup);

const FENCE = "\u0060\u0060\u0060";

const KNOWN_GOOD = [
  "## What changed",
  "",
  "Hardened the comment pipeline and added regression cover.",
  "",
  "- fixed the entity leak in prose",
  "- nested structure:",
  "  1. first nested step",
  "  2. second nested step",
  "- verified in both surfaces",
  "",
  FENCE + "go",
  "func main() {",
  "        fmt.Println(\"kept\")",
  "}",
  FENCE,
  "",
  "Inline \u0060code span\u0060 stays literal.",
  "",
  "Hard  ",
  "break with two trailing spaces.",
].join("\n");

describe("ISI-7197 — known-good sample renders cleanly", () => {
  it("renders headings, nested lists, a fence, and a hard break", () => {
    const { container } = render(<Markdown>{KNOWN_GOOD}</Markdown>);
    expect(container.querySelector("h2")!.textContent).toBe("What changed");
    const outerLis = [...container.querySelectorAll("ul > li")];
    const nested = outerLis.find((li) =>
      li.textContent!.startsWith("nested structure:"),
    );
    expect(nested).toBeTruthy();
    expect(nested!.querySelectorAll("ol > li").length).toBe(2);
    const code = container.querySelector("pre > code")!;
    expect(code).toBeTruthy();
    expect(code.textContent).toContain('fmt.Println("kept")');
    // internal fence spacing survives: 8 leading spaces on the indented line
    expect(code.textContent).toContain("\n        fmt.Println");
    expect(container.querySelector("br")).toBeTruthy();
  });

  it("emits no literal HTML entities anywhere for the clean sample", () => {
    const { container } = render(<Markdown>{KNOWN_GOOD}</Markdown>);
    expect(container.textContent).not.toContain("&#");
  });
});

describe("ISI-7197 — &#x20; entity classes (Henrik's artifact)", () => {
  it("decodes a single-escaped entity in prose — never shown literally", () => {
    const md = "sentence one.&#x20;\nsentence two.";
    const { container } = render(<Markdown>{md}</Markdown>);
    expect(container.textContent).not.toContain("&#x20;");
    expect(container.textContent).toContain("sentence one.");
    expect(container.textContent).toContain("sentence two.");
  });

  it("renders a DOUBLE-escaped entity literally (producer failure mode)", () => {
    const md = "double &amp;#x20; leak";
    const { container } = render(<Markdown>{md}</Markdown>);
    expect(container.textContent).toContain("&#x20;");
  });

  it("keeps entities inside code spans literal (CommonMark: no decoding)", () => {
    const md = "use \u0060&#x20;\u0060 carefully";
    render(<Markdown>{md}</Markdown>);
    const code = document.querySelector("code")!;
    expect(code.textContent).toBe("&#x20;");
  });

  it("keeps entities inside code fences literal (hljs escapes for HTML)", () => {
    const md = [FENCE, "pad&#x20;inside", FENCE].join("\n");
    const { container } = render(<Markdown>{md}</Markdown>);
    const code = container.querySelector("pre > code")!;
    expect(code.textContent).toContain("pad&#x20;inside");
  });
});

describe("ISI-7197 — inline code & fence structure (renderer regression)", () => {
  it("inline code stays inside the paragraph — no <pre> inside <p>", () => {
    const md = "run \u0060kubectl get pods\u0060 now";
    const { container } = render(<Markdown>{md}</Markdown>);
    const p = container.querySelector("p")!;
    expect(p.querySelector("code")).toBeTruthy();
    expect(p.querySelector("pre")).toBeNull();
    expect(container.querySelectorAll("pre").length).toBe(0);
  });

  it("a fenced block renders exactly ONE <pre> (no pre-in-pre)", () => {
    const md = [FENCE + "go", "func main() {}", FENCE].join("\n");
    const { container } = render(<Markdown>{md}</Markdown>);
    expect(container.querySelectorAll("pre").length).toBe(1);
    const pre = container.querySelector("pre")!;
    expect(pre.className).toContain("ksq-markdown__pre");
    expect(pre.querySelector("code")!.innerHTML).toContain("<span");
  });

  it("a plain fence (no language) renders one <pre> with literal text", () => {
    const md = [FENCE, "plain text", FENCE].join("\n");
    const { container } = render(<Markdown>{md}</Markdown>);
    expect(container.querySelectorAll("pre").length).toBe(1);
    expect(
      container.querySelector("pre > code")!.textContent,
    ).toContain("plain text");
  });
});
