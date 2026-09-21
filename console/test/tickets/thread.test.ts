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
  subTicketStatus,
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
      RequestedAgent: "agent:builder",
      Assignee: "agent:builder",
      statusHistory: [
        { fromState: "in_progress", toState: "in_review", principal: "agent:builder", occurredAt: "2026-09-14T10:02:00Z" },
      ],
    });
    expect(t.workItemId).toBe("wi-1");
    expect(t.title).toBe("ship it");
    expect(t.state).toBe("in_review");
    expect(t.holder).toBe("agent:builder");
    expect(t.runId).toBe("run-9");
    expect(t.requestedAgent).toBe("agent:builder");
    expect(t.assignee).toBe("agent:builder");
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
      requestedAgent: "agent:reviewer",
      assignee: "agent:builder",
    });
    expect(t.workItemId).toBe("wi-2");
    expect(t.title).toBe("camel");
    expect(t.comments[0].author).toBe("user:alice");
    expect(t.requestedAgent).toBe("agent:reviewer");
    expect(t.assignee).toBe("agent:builder");
  });

  it("degrades a null/empty payload to safe empties, never throwing", () => {
    const t = normalizeThread(null);
    expect(t.title).toBe("");
    expect(t.comments).toEqual([]);
    expect(t.changeRefs).toEqual([]);
    expect(t.statusHistory).toEqual([]);
  });

  it("normalizes requestedAgent/assignee to null when the wire omits them (ISI-4567 §2.1)", () => {
    const t = normalizeThread({ workItemId: "wi-3", title: "t", state: "backlog" });
    expect(t.requestedAgent).toBeNull();
    expect(t.assignee).toBeNull();
    // An empty-string stamp (sql NULL → "" on an older wire) also reads as null.
    const blank = normalizeThread({ RequestedAgent: "", Assignee: "" });
    expect(blank.requestedAgent).toBeNull();
    expect(blank.assignee).toBeNull();
  });
});

describe("authorKind", () => {
  it("classifies agent principals vs people", () => {
    expect(authorKind("agent:builder")).toBe("agent");
    expect(authorKind("user:alice")).toBe("user");
    expect(authorKind("henrik@perfbytes.com")).toBe("user");
  });

  it("treats a run principal as an agent (ISI-4706: progress mirror authors run/<id>)", () => {
    // The live coord progress mirror authors run steps as `run/<shortRunID>`, not
    // `agent:<name>` — these are agent executions and must NOT fall to the human
    // accent (the "everyone is blue" bug).
    expect(authorKind("run/c3beee63")).toBe("agent");
    expect(authorKind("run:c3beee63")).toBe("agent");
    // A bare human/operator principal is still a person.
    expect(authorKind("admin")).toBe("user");
    expect(authorKind("ksquad-operator")).toBe("user");
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
    requestedAgent: null,
    assignee: null,
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

describe("subTicketStatus", () => {
  it("buckets the five board states into done / in-progress / todo (ISI-4447)", () => {
    expect(
      subTicketStatus([
        { state: "done" },
        { state: "in_progress" },
        { state: "in_review" }, // in-review counts as in-progress work
        { state: "todo" },
        { state: "backlog" }, // backlog counts as todo (not-yet-started)
      ]),
    ).toEqual({ done: 1, inProgress: 2, todo: 2, total: 5 });
  });

  it("is all-zero for no sub-tickets", () => {
    expect(subTicketStatus([])).toEqual({
      done: 0,
      inProgress: 0,
      todo: 0,
      total: 0,
    });
  });
});
