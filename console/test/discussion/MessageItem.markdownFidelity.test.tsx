// test/discussion/MessageItem.markdownFidelity.test.tsx — ISI-7197 (WS-5):
// the discussion surface's settle-then-render path renders a well-formed
// agent summary cleanly — markdown structure (headings, code fences) comes
// out as rich DOM and single-escaped prose entities never surface literally.

import { describe, it, expect, afterEach } from "vitest";
import { render, screen, cleanup } from "@testing-library/react";
import { MessageItem } from "@/components/discussion/MessageItem";
import type { Message } from "@/lib/discussion/types";

afterEach(cleanup);

const FENCE = "\u0060\u0060\u0060";

function msg(over: Partial<Message> = {}): Message {
  return {
    id: "m1",
    threadId: "t1",
    parentId: null,
    authorPrincipal: "planner-1",
    authorAgentId: "agent-1",
    authorRunId: null,
    body: "hello room",
    createdAt: "2026-11-07T10:00:00Z",
    ...over,
  };
}

describe("<MessageItem> — settle-then-render fidelity (ISI-7197)", () => {
  it("renders a settled agent summary as rich markdown with no literal entities", () => {
    const body = [
      "## Summary",
      "",
      "- what was asked, what changed",
      "- decisions and verification",
      "",
      FENCE + "bash",
      "kubectl get pods",
      FENCE,
      "",
      "Done.&#x20;",
      "Next: deploy.",
    ].join("\n");
    render(
      <ul>
        <MessageItem message={msg({ body })} />
      </ul>,
    );
    const bodyEl = screen.getByTestId("message-body");
    expect(bodyEl.querySelector("h2")!.textContent).toBe("Summary");
    const code = bodyEl.querySelector("pre > code")!;
    expect(code).toBeTruthy();
    expect(code.textContent).toContain("kubectl get pods");
    expect(bodyEl.querySelectorAll("ul > li").length).toBe(2);
    // single-escaped prose entity decoded by the parser — never literal
    expect(bodyEl.textContent).not.toContain("&#x20;");
  });
});
