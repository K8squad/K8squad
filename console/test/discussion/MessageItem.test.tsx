import { describe, it, expect, afterEach } from "vitest";
import { render, screen, cleanup } from "@testing-library/react";
import { MessageItem, isRetracted } from "@/components/discussion/MessageItem";
import type { Message } from "@/lib/discussion/types";

afterEach(cleanup);

// ISI-4016: retraction is the server-stamped `invalidatedAt` column, not the
// stale `metadata.retracted` / `editedAt` shape. A retracted message renders a
// tombstone (audit-honest) instead of its body; a live one renders its body.

function msg(over: Partial<Message> = {}): Message {
  return {
    id: "m1",
    threadId: "t1",
    parentId: null,
    authorPrincipal: "planner-1",
    authorAgentId: "agent-1",
    authorRunId: null,
    body: "hello room",
    createdAt: "2026-09-09T10:00:00Z",
    ...over,
  };
}

describe("isRetracted — invalidatedAt tombstone signal", () => {
  it("true only when invalidatedAt is a non-empty timestamp", () => {
    expect(isRetracted(msg({ invalidatedAt: "2026-09-09T11:00:00Z" }))).toBe(
      true,
    );
    expect(isRetracted(msg({ invalidatedAt: null }))).toBe(false);
    expect(isRetracted(msg({ invalidatedAt: "" }))).toBe(false);
    expect(isRetracted(msg())).toBe(false);
  });
});

describe("<MessageItem> — retraction rendering", () => {
  it("renders the body for a live message", () => {
    render(
      <ul>
        <MessageItem message={msg()} />
      </ul>,
    );
    expect(screen.getByText("hello room")).toBeTruthy();
    expect(screen.queryByTestId("tombstone")).toBeNull();
  });

  it("renders a tombstone (not the body) for a retracted message", () => {
    render(
      <ul>
        <MessageItem
          message={msg({ invalidatedAt: "2026-09-09T11:00:00Z" })}
        />
      </ul>,
    );
    expect(screen.getByTestId("tombstone")).toBeTruthy();
    expect(screen.queryByText("hello room")).toBeNull();
  });

  it("agent-authored message shows an agent badge", () => {
    render(
      <ul>
        <MessageItem message={msg()} />
      </ul>,
    );
    expect(screen.getByTestId("author-chip")).toHaveAttribute(
      "data-kind",
      "agent",
    );
  });
});
