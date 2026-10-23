// test/markdown/Markdown.test.tsx — ISI-5780: shared <Markdown> renderer.

import { describe, it, expect, afterEach } from "vitest";
import { render, screen, cleanup } from "@testing-library/react";
import { Markdown } from "@/components/markdown/Markdown";

afterEach(cleanup);

// ── GFM ───────────────────────────────────────────────────────────────────

describe("GFM — tables", () => {
  it("renders a GFM table", () => {
    const md = `| A | B |\n|---|---|\n| 1 | 2 |`;
    render(<Markdown>{md}</Markdown>);
    expect(screen.getByRole("table")).toBeTruthy();
    expect(screen.getByText("A")).toBeTruthy();
    expect(screen.getByText("1")).toBeTruthy();
  });
});

describe("GFM — strikethrough", () => {
  it("renders strikethrough text inside <del>", () => {
    render(<Markdown>{"~~gone~~"}</Markdown>);
    expect(document.querySelector("del")).toBeTruthy();
  });
});

describe("GFM — task lists", () => {
  it("renders checkboxes for task list items", () => {
    const md = `- [x] done\n- [ ] todo`;
    render(<Markdown>{md}</Markdown>);
    const checkboxes = document.querySelectorAll('input[type="checkbox"]');
    expect(checkboxes.length).toBe(2);
    expect((checkboxes[0] as HTMLInputElement).checked).toBe(true);
    expect((checkboxes[1] as HTMLInputElement).checked).toBe(false);
  });
});

// ── Code fences ───────────────────────────────────────────────────────────

describe("code fences", () => {
  it("renders a fenced code block in a <pre><code>", () => {
    const md = '```go\nfmt.Println("hello")\n```';
    render(<Markdown>{md}</Markdown>);
    const pre = document.querySelector("pre");
    expect(pre).toBeTruthy();
    const code = pre!.querySelector("code");
    expect(code).toBeTruthy();
  });

  it("syntax-highlights Go code (hljs injects span markup)", () => {
    const md = "```go\nfunc main() {}\n```";
    render(<Markdown>{md}</Markdown>);
    const code = document.querySelector("pre code");
    expect(code!.innerHTML).toContain("<span");
  });
});

// ── Links ─────────────────────────────────────────────────────────────────

describe("links", () => {
  it("adds target=_blank + rel=noopener for http links", () => {
    render(<Markdown>{"[ext](https://example.com)"}</Markdown>);
    const a = document.querySelector("a")!;
    expect(a.getAttribute("target")).toBe("_blank");
    expect(a.getAttribute("rel")).toBe("noopener noreferrer");
  });

  it("does NOT add target=_blank for relative links", () => {
    render(<Markdown>{"[local](/foo/bar)"}</Markdown>);
    const a = document.querySelector("a")!;
    expect(a.getAttribute("target")).toBeNull();
  });
});

// ── XSS hardening ─────────────────────────────────────────────────────────

describe("XSS hardening", () => {
  it("does not execute a raw <script> tag embedded in markdown", () => {
    const md = "hello <script>window.__xss=1</script> world";
    render(<Markdown>{md}</Markdown>);
    // No rehype-raw → raw HTML is treated as escaped text, never as DOM nodes.
    expect(document.querySelector("script")).toBeNull();
    expect(
      (window as unknown as Record<string, unknown>).__xss,
    ).toBeUndefined();
  });

  it("does not render an <img onerror> as a live element", () => {
    const md = '<img src="x" onerror="window.__xss2=1">';
    render(<Markdown>{md}</Markdown>);
    expect(document.querySelector("img")).toBeNull();
    expect(
      (window as unknown as Record<string, unknown>).__xss2,
    ).toBeUndefined();
  });

  it("renders javascript: href as inert text, not a live anchor", () => {
    // react-markdown sanitises javascript: hrefs — the anchor is either dropped
    // or rendered without the dangerous href.
    const md = "[click](javascript:alert(1))";
    render(<Markdown>{md}</Markdown>);
    const a = document.querySelector("a");
    if (a) {
      const href = a.getAttribute("href") ?? "";
      expect(href.toLowerCase().startsWith("javascript:")).toBe(false);
    }
    // Either no anchor or a sanitised one — both are acceptable.
  });
});

// ── Oversize fallback ─────────────────────────────────────────────────────

describe("oversize fallback", () => {
  it("falls back to pre-wrap plain text when content exceeds 1 MiB", () => {
    // Build a string that encodes to >1 MiB in UTF-8.
    const big = "a".repeat(1_100_000);
    render(<Markdown testId="md">{big}</Markdown>);
    const el = screen.getByTestId("md");
    expect(el.tagName.toLowerCase()).toBe("pre");
    expect(el.className).toContain("ksq-markdown--plain");
  });
});
