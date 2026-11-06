// test/discussion/twoTierThinking.test.tsx — ISI-7101 (WS-4 of the ISI-7074
// comment-quality plan): the console renders the Paperclip TWO-TIER experience —
// live italic thinking while a run works, then ONE clean comment.
//
// Proves the three legs of the acceptance on the discussion seam:
//   • the ISI-7098 wire tier survives the parse (kind 'thinking' | 'deliverable';
//     absent/garbage decodes as the legacy thinking tier, per PR #977's rule);
//   • <ThinkingStream> is OPEN + "working…" + italic while the run is active,
//     and falls back to the collapsed "Run activity" affordance once settled
//     (the ISI-5696 demotion holds for the cold/later reader);
//   • a live 'deliverable' envelope (the run ONE final answer arriving ahead of
//     the durable reply) renders UPRIGHT in its own list, never in the italic
//     thinking stream;
//   • <MessageItem> derives active from the message's dispatch watches, so the
//     room needs no new plumbing to flip the tiers at completion.

import { describe, it, expect, afterEach } from "vitest";
import { render, screen, cleanup } from "@testing-library/react";
import { parseThinkingRow, type ThinkingRow } from "@/lib/discussion/liveFeed";
import { mergeLiveThinking, buildActivity } from "@/lib/tickets/thread";
import { ThinkingStream } from "@/components/discussion/ThinkingStream";
import { MessageItem } from "@/components/discussion/MessageItem";
import type { Message } from "@/lib/discussion/types";
import type { DispatchWatch } from "@/lib/discussion/working";

afterEach(cleanup);

function envelope(over: Record<string, unknown> = {}): string {
  return JSON.stringify({
    runId: "run-1",
    author: "agent:sam",
    body: "[run abc] reading the seam",
    seq: 3,
    at: "2026-11-06T12:00:00Z",
    ...over,
  });
}

function row(over: Partial<ThinkingRow> = {}): ThinkingRow {
  return {
    runId: "run-1",
    author: "agent:sam",
    body: "[run abc] reading the seam",
    seq: 3,
    at: "2026-11-06T12:00:00Z",
    ...over,
  };
}

describe("parseThinkingRow — the ISI-7098 wire tier (ISI-7101)", () => {
  it("keeps a stamped thinking tier", () => {
    expect(parseThinkingRow(envelope({ kind: "thinking" }))?.kind).toBe(
      "thinking",
    );
  });

  it("keeps a stamped deliverable tier (the run ONE final answer)", () => {
    expect(parseThinkingRow(envelope({ kind: "deliverable" }))?.kind).toBe(
      "deliverable",
    );
  });

  it("decodes an absent tier as legacy thinking (undefined, rendered thinking)", () => {
    const parsed = parseThinkingRow(envelope());
    expect(parsed?.kind).toBeUndefined();
  });

  it("decodes an unknown tier value as legacy thinking, never throws", () => {
    expect(parseThinkingRow(envelope({ kind: "musing" }))?.kind).toBeUndefined();
  });
});

describe("mergeLiveThinking — live rows carry the two-tier flags (ISI-7101)", () => {
  const base = buildActivity({
    workItemId: "w",
    title: "t",
    description: "",
    state: "todo",
    blockedReason: "",
    comments: [
      { author: "user:u", body: "please look", createdAt: "2026-11-06T11:00:00Z" },
    ],
    changeRefs: [],
    statusHistory: [],
    holder: "",
    runId: "",
    requestedAgent: null,
    assignee: null,
  });

  it("marks a live row live with the thinking tier by default", () => {
    const merged = mergeLiveThinking(base, [
      { author: "agent:sam", body: "thinking out loud", at: "2026-11-06T12:00:00Z" },
    ]);
    const live = merged.find((i) => i.live);
    expect(live?.live).toBe(true);
    expect(live?.liveTier).toBe("thinking");
  });

  it("carries a stamped deliverable tier through as the clean-comment tier", () => {
    const merged = mergeLiveThinking(base, [
      {
        author: "agent:sam",
        body: "final answer",
        at: "2026-11-06T12:00:00Z",
        kind: "deliverable",
      },
    ]);
    const live = merged.find((i) => i.live);
    expect(live?.live).toBe(true);
    expect(live?.liveTier).toBe("deliverable");
  });

  it("never flags a durable row as live", () => {
    expect(base.every((i) => !i.live)).toBe(true);
  });
});

describe("<ThinkingStream> — live italic tier vs clean deliverable (ISI-7101)", () => {
  it("renders OPEN with the working… label while the run is active", () => {
    render(<ThinkingStream rows={[row()]} active={true} />);
    const details = screen.getByTestId("activity-details");
    expect(details.hasAttribute("open")).toBe(true);
    expect(screen.getByTestId("activity-toggle").textContent).toContain(
      "working…",
    );
  });

  it("renders COLLAPSED with the Run activity label once the run settled", () => {
    render(<ThinkingStream rows={[row()]} active={false} />);
    const details = screen.getByTestId("activity-details");
    expect(details.hasAttribute("open")).toBe(false);
    expect(screen.getByTestId("activity-toggle").textContent).toContain(
      "Run activity",
    );
  });

  it("streams thinking rows inside the italic thinking list", () => {
    render(<ThinkingStream rows={[row()]} active={true} />);
    expect(screen.getAllByTestId("thinking-row")).toHaveLength(1);
    expect(screen.queryByTestId("thinking-final-row")).toBeNull();
  });

  it("renders a deliverable row UPRIGHT in its own final list, out of the thinking list", () => {
    render(
      <ThinkingStream
        rows={[row(), row({ seq: 4, body: "done: the fix landed", kind: "deliverable" })]}
        active={true}
      />,
    );
    expect(screen.getAllByTestId("thinking-row")).toHaveLength(1);
    const final = screen.getAllByTestId("thinking-final-row");
    expect(final).toHaveLength(1);
    expect(final[0].textContent).toContain("done: the fix landed");
  });

  it("renders nothing without rows", () => {
    const { container } = render(<ThinkingStream rows={[]} active={true} />);
    expect(container.querySelector("details")).toBeNull();
  });
});

describe("<MessageItem> derives the live tier from its dispatch watches (ISI-7101)", () => {
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

  function watch(phase: DispatchWatch["phase"]): DispatchWatch {
    return {
      key: "m1:sam",
      agentName: "sam",
      messageId: "m1",
      phase,
      since: 1,
    };
  }

  const thinking = { m1: [row()] };

  it("opens the stream while a dispatched run is still working", () => {
    render(
      <ul>
        <MessageItem
          message={msg()}
          thinkingByMessageId={thinking}
          workingByMessageId={{ m1: [watch("working")] }}
        />
      </ul>,
    );
    expect(screen.getByTestId("activity-details").hasAttribute("open")).toBe(
      true,
    );
  });

  it("collapses the stream once the watch settled (replied) — the reply is the clean comment", () => {
    render(
      <ul>
        <MessageItem
          message={msg()}
          thinkingByMessageId={thinking}
          workingByMessageId={{ m1: [watch("replied")] }}
        />
      </ul>,
    );
    expect(screen.getByTestId("activity-details").hasAttribute("open")).toBe(
      false,
    );
  });

  it("stays collapsed with no watches at all (the later-reader view)", () => {
    render(
      <ul>
        <MessageItem message={msg()} thinkingByMessageId={thinking} />
      </ul>,
    );
    expect(screen.getByTestId("activity-details").hasAttribute("open")).toBe(
      false,
    );
  });
});
