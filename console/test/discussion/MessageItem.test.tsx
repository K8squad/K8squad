import { describe, it, expect, afterEach } from "vitest";
import { render, screen, cleanup } from "@testing-library/react";
import {
  MessageItem,
  isRetracted,
  parseReferences,
} from "@/components/discussion/MessageItem";
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

describe("<MessageItem> — audience chip (ISI-4929, plan §4.2)", () => {
  it("a direct message renders a direct chip and data-audience=direct", () => {
    render(
      <ul>
        <MessageItem message={msg({ audience: "direct:agent-7" })} />
      </ul>,
    );
    const chip = screen.getByTestId("audience-chip");
    expect(chip.textContent).toBe("direct");
    expect(screen.getByTestId("message")).toHaveAttribute(
      "data-audience",
      "direct",
    );
  });

  it("a party message (and pre-v2 messages with no audience) render no chip", () => {
    const { rerender } = render(
      <ul>
        <MessageItem message={msg({ audience: "party" })} />
      </ul>,
    );
    expect(screen.queryByTestId("audience-chip")).toBeNull();
    expect(screen.getByTestId("message")).toHaveAttribute(
      "data-audience",
      "party",
    );
    rerender(
      <ul>
        <MessageItem message={msg({ audience: undefined })} />
      </ul>,
    );
    expect(screen.queryByTestId("audience-chip")).toBeNull();
    expect(screen.getByTestId("message")).toHaveAttribute(
      "data-audience",
      "party",
    );
  });
});

// ISI-5168 (plan ISI-5134 S4): ticket references stamped onto a message payload
// (ISI-5165 `payload.references`) linkify as chips beneath the body, read
// straight off the stored ref (UUID + title) — no render-time refetch.

describe("parseReferences — payload.references reader", () => {
  it("reads well-formed refs and drops malformed entries", () => {
    expect(
      parseReferences({
        references: [
          { workItemId: "wi-1", title: "Fix login", state: "in_progress" },
          { workItemId: "wi-2" },
          { title: "no id" },
          { workItemId: "" },
          null,
          "nope",
        ],
      }),
    ).toEqual([
      { workItemId: "wi-1", title: "Fix login", state: "in_progress" },
      { workItemId: "wi-2", title: undefined, state: undefined },
    ]);
  });

  it("returns [] for absent/malformed payloads", () => {
    expect(parseReferences(undefined)).toEqual([]);
    expect(parseReferences(null)).toEqual([]);
    expect(parseReferences({})).toEqual([]);
    expect(parseReferences({ references: "nope" })).toEqual([]);
  });
});

describe("<MessageItem> — ticket reference chips (ISI-5168)", () => {
  it("renders a ticket link to /projects/{projectId}/issues/{workItemId} with the title", () => {
    render(
      <ul>
        <MessageItem
          projectId="proj-9"
          message={msg({
            payload: {
              references: [{ workItemId: "wi-42", title: "Fix login" }],
            },
          })}
        />
      </ul>,
    );
    const link = screen.getByTestId("ticket-ref");
    expect(link.tagName).toBe("A");
    expect(link).toHaveAttribute(
      "href",
      "/projects/proj-9/issues/wi-42",
    );
    expect(link.textContent).toContain("Fix login");
  });

  it("surfaces the optional state and still renders the body", () => {
    render(
      <ul>
        <MessageItem
          projectId="proj-9"
          message={msg({
            body: "see this ticket",
            payload: {
              references: [
                { workItemId: "wi-7", title: "Ship it", state: "in_review" },
              ],
            },
          })}
        />
      </ul>,
    );
    expect(screen.getByText("see this ticket")).toBeTruthy();
    const link = screen.getByTestId("ticket-ref");
    expect(link.textContent).toContain("Ship it");
    expect(link.textContent).toContain("in_review");
  });

  it("renders unchanged (no ticket chips) for a message with no references", () => {
    render(
      <ul>
        <MessageItem projectId="proj-9" message={msg()} />
      </ul>,
    );
    expect(screen.getByText("hello room")).toBeTruthy();
    expect(screen.queryByTestId("ticket-refs")).toBeNull();
    expect(screen.queryByTestId("ticket-ref")).toBeNull();
  });

  it("renders a presence-only span (no dead link) when projectId is missing", () => {
    render(
      <ul>
        <MessageItem
          message={msg({
            payload: {
              references: [{ workItemId: "wi-42", title: "Fix login" }],
            },
          })}
        />
      </ul>,
    );
    const chip = screen.getByTestId("ticket-ref");
    expect(chip.tagName).toBe("SPAN");
    expect(chip.textContent).toContain("Fix login");
  });
});
