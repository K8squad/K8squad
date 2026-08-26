import { describe, it, expect, vi, afterEach } from "vitest";
import {
  fetchSearch,
  resultHref,
  sanitizeSnippet,
  stateBadge,
  SearchError,
  type SearchResponse,
  type SearchResult,
} from "@/lib/search";

// Pure + edge contract of the 8.19 search data layer: the <mark>-only sanitizer (XSS gate), the
// fetch URL/param shape + blank-query guard the BFF forwards verbatim, status → SearchError, and the
// honest state/href derivations.

function result(over: Partial<SearchResult> = {}): SearchResult {
  return {
    type: "work_item",
    id: "6f000000-0000-0000-0000-000000000001",
    projectId: "11111111-1111-1111-1111-111111111111",
    title: "Fix checkout bug",
    snippet: "…text with a <mark>checkout</mark> hit…",
    state: "in_progress",
    rank: 0.83,
    updatedAt: "2026-08-25T10:00:00Z",
    ...over,
  };
}

afterEach(() => {
  vi.restoreAllMocks();
});

describe("sanitizeSnippet — the <mark>-only XSS gate", () => {
  it("keeps the server's <mark>…</mark> highlight tags", () => {
    expect(sanitizeSnippet("a <mark>hit</mark> here")).toBe("a <mark>hit</mark> here");
  });

  it("escapes an injected <img onerror> — only <mark> survives, script never renders as html", () => {
    const evil = `<img src=x onerror="alert(1)"> and <mark>ok</mark> and <script>alert(2)</script>`;
    const out = sanitizeSnippet(evil);
    // The dangerous tags are inert escaped text…
    expect(out).toContain("&lt;img");
    expect(out).toContain("onerror=");
    expect(out).toContain("&lt;script&gt;");
    expect(out).not.toContain("<img");
    expect(out).not.toContain("<script>");
    // …while the one intended highlight tag is admitted.
    expect(out).toContain("<mark>ok</mark>");
  });

  it("escapes quotes and ampersands so attribute/entity breakouts can't form", () => {
    expect(sanitizeSnippet(`a & b "c" 'd'`)).toBe("a &amp; b &quot;c&quot; &#39;d&#39;");
  });

  it("a fake nested mark with attributes stays escaped (only the bare tag is admitted)", () => {
    const out = sanitizeSnippet(`<mark onmouseover="x">y</mark>`);
    expect(out).toContain("&lt;mark onmouseover=");
    expect(out).toContain("</mark>");
    expect(out).not.toContain('<mark onmouseover');
  });
});

describe("fetchSearch — URL shape, guard, parsing, status", () => {
  it("blank / whitespace query is guarded — returns empty WITHOUT touching the network", async () => {
    const spy = vi.spyOn(globalThis, "fetch");
    await expect(fetchSearch("   ")).resolves.toEqual({ query: "", results: [] });
    await expect(fetchSearch("")).resolves.toEqual({ query: "", results: [] });
    expect(spy).not.toHaveBeenCalled();
  });

  it("builds /api/search?q=&limit= (trimmed, default limit 8) and parses the payload", async () => {
    const payload: SearchResponse = { query: "checkout bug", results: [result()] };
    const spy = vi.spyOn(globalThis, "fetch").mockResolvedValue(
      new Response(JSON.stringify(payload), {
        status: 200,
        headers: { "content-type": "application/json" },
      }),
    );
    const out = await fetchSearch("  checkout bug  ");
    const url = spy.mock.calls[0][0] as string;
    expect(url).toBe("/api/search?q=checkout+bug&limit=8");
    expect(out.query).toBe("checkout bug");
    expect(out.results).toHaveLength(1);
    expect(out.results[0].title).toBe("Fix checkout bug");
  });

  it("honors an explicit limit", async () => {
    const spy = vi.spyOn(globalThis, "fetch").mockResolvedValue(
      new Response(JSON.stringify({ query: "x", results: [] }), { status: 200 }),
    );
    await fetchSearch("x", 20);
    expect(spy.mock.calls[0][0]).toBe("/api/search?q=x&limit=20");
  });

  it("a non-2xx (deny/400/5xx) throws SearchError carrying the verbatim status", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response("nope", { status: 403 }));
    await expect(fetchSearch("secret")).rejects.toBeInstanceOf(SearchError);
    await expect(fetchSearch("secret")).rejects.toMatchObject({ status: 403 });
  });

  it("tolerates a malformed body (missing results) — never throws on shape", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(
      new Response(JSON.stringify({ query: "x" }), { status: 200 }),
    );
    await expect(fetchSearch("x")).resolves.toEqual({ query: "x", results: [] });
  });
});

describe("derivations", () => {
  it("stateBadge maps known states and passes unknown through verbatim", () => {
    expect(stateBadge("in_progress")).toEqual({ label: "In progress", tone: "info" });
    expect(stateBadge("done")).toEqual({ label: "Done", tone: "ok" });
    expect(stateBadge("mystery")).toEqual({ label: "mystery", tone: "idle" });
  });

  it("resultHref deep-links Project-scoped hits to Tickets and falls back to Overview", () => {
    expect(resultHref(result({ projectId: "p1", id: "w1" }))).toBe(
      "/projects/p1/tickets?item=w1",
    );
    expect(resultHref(result({ projectId: "", id: "w1" }))).toBe("/overview");
  });

  it("resultHref percent-encodes ids into the path/query", () => {
    expect(resultHref(result({ projectId: "a/b", id: "c d" }))).toBe(
      "/projects/a%2Fb/tickets?item=c%20d",
    );
  });
});
