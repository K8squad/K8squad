// Component-boundary coverage for the collapsible Discussions panel
// (ISI-5673 / ISI-5598 WS-C): row rendering + active highlight, open-existing
// selection, inline new-chat (Untitled default + Enter/Esc), collapse toggle
// with localStorage persistence, and honest loading/empty/error states.

import { describe, it, expect, vi, afterEach, beforeEach } from "vitest";
import {
  render,
  screen,
  cleanup,
  fireEvent,
  within,
} from "@testing-library/react";
import {
  DiscussionsPanel,
  PANEL_EXPAND_KEY,
} from "@/components/discussion/DiscussionsPanel";
import type { Thread } from "@/lib/discussion/types";

afterEach(cleanup);
beforeEach(() => {
  try {
    localStorage.clear();
  } catch {
    /* ignore */
  }
});

function thread(id: string, title: string, createdAt: string): Thread {
  return {
    id,
    projectId: "p1",
    teamId: "t1",
    title,
    createdBy: "alice",
    createdAt,
  };
}

const NOW = Date.parse("2026-10-12T12:00:00Z");
const THREADS = [
  thread("newest", "Planning", "2026-10-12T11:30:00Z"),
  thread("older", "Retro", "2026-10-10T12:00:00Z"),
];

function renderPanel(
  over: Partial<React.ComponentProps<typeof DiscussionsPanel>> = {},
) {
  const props = {
    threads: THREADS,
    activeThreadId: "newest",
    listState: "ready" as const,
    onSelect: vi.fn(),
    onNewChat: vi.fn(),
    onRetry: vi.fn(),
    now: NOW,
    ...over,
  };
  render(<DiscussionsPanel {...props} />);
  return props;
}

describe("<DiscussionsPanel> — ISI-5673", () => {
  it("renders a row per thread with title, relative timestamp, and active highlight", () => {
    renderPanel();
    const list = screen.getByTestId("discussions-list");
    expect(within(list).getAllByRole("listitem")).toHaveLength(2);
    expect(screen.getByText("Planning")).toBeTruthy();
    expect(screen.getByText("30m ago")).toBeTruthy();
    // The active row carries aria-current="page"; the inactive one does not.
    const active = screen.getByTestId("discussions-row-newest");
    expect(active.getAttribute("aria-current")).toBe("page");
    expect(active.getAttribute("data-active")).toBe("true");
    expect(
      screen.getByTestId("discussions-row-older").getAttribute("aria-current"),
    ).toBeNull();
  });

  it("the row href is the linkable ?thread query", () => {
    renderPanel();
    expect(
      screen.getByTestId("discussions-row-older").getAttribute("href"),
    ).toBe("?thread=older");
  });

  it("plain click selects a thread (in-app); the href still powers modified clicks", () => {
    const { onSelect } = renderPanel();
    fireEvent.click(screen.getByTestId("discussions-row-older"));
    expect(onSelect).toHaveBeenCalledWith("older");
  });

  it("a modified (new-tab) click does NOT intercept — the browser follows the href", () => {
    const { onSelect } = renderPanel();
    fireEvent.click(screen.getByTestId("discussions-row-older"), {
      metaKey: true,
    });
    expect(onSelect).not.toHaveBeenCalled();
  });

  it("New chat opens an inline Untitled field; Enter commits the typed title", () => {
    const { onNewChat } = renderPanel();
    fireEvent.click(screen.getByTestId("discussions-new-chat"));
    const input = screen.getByTestId(
      "discussions-new-chat-input",
    ) as HTMLInputElement;
    expect(input.placeholder).toBe("Untitled");
    fireEvent.change(input, { target: { value: "Release prep" } });
    fireEvent.keyDown(input, { key: "Enter" });
    expect(onNewChat).toHaveBeenCalledWith("Release prep");
  });

  it("an empty New-chat title commits as 'Untitled'", () => {
    const { onNewChat } = renderPanel();
    fireEvent.click(screen.getByTestId("discussions-new-chat"));
    const input = screen.getByTestId("discussions-new-chat-input");
    fireEvent.keyDown(input, { key: "Enter" });
    expect(onNewChat).toHaveBeenCalledWith("Untitled");
  });

  it("Esc cancels the new-chat draft without creating a thread", () => {
    const { onNewChat } = renderPanel();
    fireEvent.click(screen.getByTestId("discussions-new-chat"));
    fireEvent.keyDown(screen.getByTestId("discussions-new-chat-input"), {
      key: "Escape",
    });
    expect(onNewChat).not.toHaveBeenCalled();
    expect(screen.queryByTestId("discussions-new-chat-draft")).toBeNull();
  });

  it("collapse toggle flips aria-expanded and persists to localStorage", () => {
    renderPanel({ defaultExpanded: true });
    const panel = screen.getByTestId("discussions-panel");
    const toggle = screen.getByTestId("discussions-panel-toggle");
    expect(toggle.getAttribute("aria-expanded")).toBe("true");
    expect(panel.getAttribute("data-collapsed")).toBeNull();

    fireEvent.click(toggle);
    expect(toggle.getAttribute("aria-expanded")).toBe("false");
    expect(panel.getAttribute("data-collapsed")).toBe("true");
    expect(localStorage.getItem(PANEL_EXPAND_KEY)).toBe("0");
  });

  it("restores the persisted collapsed state on mount", () => {
    localStorage.setItem(PANEL_EXPAND_KEY, "0");
    renderPanel({ defaultExpanded: undefined });
    expect(
      screen
        .getByTestId("discussions-panel-toggle")
        .getAttribute("aria-expanded"),
    ).toBe("false");
  });

  it("renders honest loading / empty / error states", () => {
    const { rerender } = render(
      <DiscussionsPanel
        threads={[]}
        activeThreadId={null}
        listState="loading"
        onSelect={vi.fn()}
        onNewChat={vi.fn()}
        onRetry={vi.fn()}
        now={NOW}
      />,
    );
    expect(screen.getByTestId("discussions-panel-loading")).toBeTruthy();

    rerender(
      <DiscussionsPanel
        threads={[]}
        activeThreadId={null}
        listState="ready"
        onSelect={vi.fn()}
        onNewChat={vi.fn()}
        onRetry={vi.fn()}
        now={NOW}
      />,
    );
    expect(screen.getByTestId("discussions-panel-empty")).toBeTruthy();

    const onRetry = vi.fn();
    rerender(
      <DiscussionsPanel
        threads={[]}
        activeThreadId={null}
        listState="error"
        onSelect={vi.fn()}
        onNewChat={vi.fn()}
        onRetry={onRetry}
        now={NOW}
      />,
    );
    fireEvent.click(
      within(screen.getByTestId("discussions-panel-error")).getByText("Retry"),
    );
    expect(onRetry).toHaveBeenCalled();
  });

  it("collapsed rail shows lettered tiles and an icon-only new-chat", () => {
    localStorage.setItem(PANEL_EXPAND_KEY, "0");
    renderPanel({ defaultExpanded: undefined });
    // Title heading hidden; tile shows the first letter.
    expect(screen.queryByText("Discussions")).toBeNull();
    const activeRow = screen.getByTestId("discussions-row-newest");
    expect(within(activeRow).getByText("P")).toBeTruthy(); // "Planning"
  });
});
