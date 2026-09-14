// test/tickets/thread.test.ts — the ticket-detail read model shaping (ISI-4399
// S3). normalizeThread is the single tolerant boundary over the apiserver's
// mixed-case wire shape (PascalCase TaskDetail fields + camelCase nested arrays,
// workitemread.go); buildActivity is the chronological merge the thread renders.

import { describe, it, expect } from "vitest";
import {
  authorKind,
  buildActivity,
  normalizeThread,
  subTicketProgress,
  type NormalizedThread,
} from "@/lib/tickets/thread";

describe("normalizeThread", () => {
  it("reads the server's PascalCase TaskDetail shape", () => {
    const t = normalizeThread({
      WorkItemID: "wi-1",
      Title: "ship it",
      Description: "do the thing",
      State: "in_review",
      BlockedReason: "",
      Comments: [{ author: "agent:builder", body: "seam wired", createdAt: "2026-09-14T10:00:00Z" }],
      ChangeRefs: [{ kind: "commit", ref: "deadbeef", author: "agent:builder", createdAt: "2026-09-14T10:01:00Z" }],
      Holder: "agent:builder",
      RunID: "run-9",
      statusHistory: [
        { fromState: "in_progress", toState: "in_review", principal: "agent:builder", occurredAt: "2026-09-14T10:02:00Z" },
      ],
    });
    expect(t.workItemId).toBe("wi-1");
    expect(t.title).toBe("ship it");
    expect(t.state).toBe("in_review");
    expect(t.holder).toBe("agent:builder");
    expect(t.runId).toBe("run-9");
    expect(t.comments).toHaveLength(1);
    expect(t.changeRefs[0].ref).toBe("deadbeef");
    expect(t.statusHistory[0].toState).toBe("in_review");
  });

  it("also tolerates a camelCase shape (future-proof)", () => {
    const t = normalizeThread({
      workItemId: "wi-2",
      title: "camel",
      description: "d",
      state: "todo",
      comments: [{ author: "user:alice", body: "hi", createdAt: "2026-09-14T09:00:00Z" }],
    });
    expect(t.workItemId).toBe("wi-2");
    expect(t.title).toBe("camel");
    expect(t.comments[0].author).toBe("user:alice");
  });

  it("degrades a null/empty payload to safe empties, never throwing", () => {
    const t = normalizeThread(null);
    expect(t.title).toBe("");
    expect(t.comments).toEqual([]);
    expect(t.changeRefs).toEqual([]);
    expect(t.statusHistory).toEqual([]);
  });
});

describe("authorKind", () => {
  it("classifies agent principals vs people", () => {
    expect(authorKind("agent:builder")).toBe("agent");
    expect(authorKind("user:alice")).toBe("user");
    expect(authorKind("henrik@perfbytes.com")).toBe("user");
  });
});

function thread(partial: Partial<NormalizedThread>): NormalizedThread {
  return {
    workItemId: "wi",
    title: "t",
    description: "",
    state: "todo",
    blockedReason: "",
    comments: [],
    changeRefs: [],
    statusHistory: [],
    holder: "",
    runId: "",
    ...partial,
  };
}

describe("buildActivity", () => {
  it("merges comments + events + changes chronologically, newest last", () => {
    const items = buildActivity(
      thread({
        comments: [{ author: "u", body: "b", createdAt: "2026-09-14T10:00:00Z" }],
        statusHistory: [
          { fromState: "todo", toState: "in_progress", principal: "agent:x", occurredAt: "2026-09-14T09:00:00Z" },
        ],
        changeRefs: [{ kind: "commit", ref: "abc", author: "agent:x", createdAt: "2026-09-14T11:00:00Z" }],
      }),
    );
    expect(items.map((i) => i.kind)).toEqual(["event", "comment", "change"]);
  });

  it("sorts an undated row to the end (treated as just-now, not epoch-0)", () => {
    const items = buildActivity(
      thread({
        comments: [
          { author: "u", body: "dated", createdAt: "2026-09-14T10:00:00Z" },
          { author: "u", body: "undated", createdAt: "" },
        ],
      }),
    );
    expect(items[items.length - 1].comment?.body).toBe("undated");
  });
});

describe("subTicketProgress", () => {
  it("counts done vs total", () => {
    expect(
      subTicketProgress([{ state: "done" }, { state: "todo" }, { state: "done" }]),
    ).toEqual({ done: 2, total: 3 });
    expect(subTicketProgress([])).toEqual({ done: 0, total: 0 });
  });
});
