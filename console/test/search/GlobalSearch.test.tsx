import { describe, it, expect, vi, afterEach, beforeEach } from "vitest";
import { render, screen, cleanup, waitFor, fireEvent } from "@testing-library/react";
import { GlobalSearch } from "@/components/search/GlobalSearch";
import type { SearchResponse, SearchResult } from "@/lib/search";

// next/navigation's useRouter needs the App Router context (absent in jsdom) — mock it so Enter /
// click navigation is observable without a Next runtime.
const push = vi.fn();
vi.mock("next/navigation", () => ({
  useRouter: () => ({ push }),
}));

afterEach(cleanup);
beforeEach(() => push.mockClear());

function result(over: Partial<SearchResult> = {}): SearchResult {
  return {
    type: "work_item",
    id: "w1",
    projectId: "p1",
    title: "Fix checkout bug",
    snippet: "…a <mark>checkout</mark> hit…",
    state: "in_progress",
    rank: 0.8,
    updatedAt: "2026-08-25T10:00:00Z",
    ...over,
  };
}

function ok(results: SearchResult[]): SearchResponse {
  return { query: "q", results };
}

function type(value: string) {
  fireEvent.change(screen.getByTestId("global-search-input"), { target: { value } });
}

describe("<GlobalSearch> — 8.19 ACs", () => {
  it("typing triggers a (debounced) search and renders ranked hits with sanitized <mark>", async () => {
    const search = vi.fn(async () => ok([result(), result({ id: "w2", title: "Checkout flaky test", state: "todo" })]));
    render(<GlobalSearch search={search} />);

    type("checkout");
    await waitFor(() => expect(search).toHaveBeenCalledTimes(1));
    expect(search).toHaveBeenCalledWith("checkout", 8, expect.anything());

    await waitFor(() => expect(screen.getAllByTestId("global-search-option")).toHaveLength(2));
    // The server highlight <mark> is rendered as real markup (sanitized), not escaped text.
    expect(screen.getByTestId("global-search-list").querySelector("mark")?.textContent).toBe(
      "checkout",
    );
    // combobox exposes the listbox via aria.
    expect(screen.getByRole("combobox").getAttribute("aria-expanded")).toBe("true");
  });

  it("a blank / whitespace query never calls the API and shows no panel", async () => {
    const search = vi.fn(async () => ok([result()]));
    render(<GlobalSearch search={search} />);

    type("   ");
    // Give the debounce window a chance to (not) fire.
    await new Promise((r) => setTimeout(r, 260));
    expect(search).not.toHaveBeenCalled();
    expect(screen.queryByTestId("global-search-panel")).toBeNull();
  });

  it("an empty result set renders the honest no-match state", async () => {
    const search = vi.fn(async () => ok([]));
    render(<GlobalSearch search={search} />);
    type("zzz");
    await waitFor(() => screen.getByTestId("global-search-empty"));
  });

  it("an upstream failure surfaces a discreet inline error, never crashes", async () => {
    const search = vi.fn(async () => {
      throw new Error("boom");
    });
    render(<GlobalSearch search={search} />);
    type("bad");
    await waitFor(() => screen.getByTestId("global-search-error"));
  });

  it("Arrow keys move the active option and Enter navigates the highlighted hit", async () => {
    const search = vi.fn(async () => ok([result({ id: "w1", projectId: "p1" }), result({ id: "w2", projectId: "p1" })]));
    render(<GlobalSearch search={search} />);
    const input = screen.getByTestId("global-search-input");
    type("checkout");
    await waitFor(() => expect(screen.getAllByTestId("global-search-option")).toHaveLength(2));

    fireEvent.keyDown(input, { key: "ArrowDown" }); // → option 0
    fireEvent.keyDown(input, { key: "ArrowDown" }); // → option 1
    await waitFor(() =>
      expect(input.getAttribute("aria-activedescendant")).toBe(
        screen.getAllByTestId("global-search-option")[1].id,
      ),
    );

    fireEvent.keyDown(input, { key: "Enter" });
    expect(push).toHaveBeenCalledWith("/projects/p1/tickets?item=w2");
  });

  it("Escape closes the dropdown", async () => {
    const search = vi.fn(async () => ok([result()]));
    render(<GlobalSearch search={search} />);
    const input = screen.getByTestId("global-search-input");
    type("checkout");
    await waitFor(() => screen.getByTestId("global-search-panel"));

    fireEvent.keyDown(input, { key: "Escape" });
    await waitFor(() => expect(screen.queryByTestId("global-search-panel")).toBeNull());
  });

  it('the "/" hotkey focuses the search box from anywhere on the page', async () => {
    render(<GlobalSearch search={vi.fn(async () => ok([]))} />);
    const input = screen.getByTestId("global-search-input");
    expect(document.activeElement).not.toBe(input);
    fireEvent.keyDown(window, { key: "/" });
    expect(document.activeElement).toBe(input);
  });
});
