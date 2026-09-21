// test/tickets/runComments.test.ts — the S3 (ISI-4449) run-comment shaping rules:
// newest last, run meta attributed ONLY to the newest agent bubble (the current
// holding run), the live dot gated on an in-flight phase, and no fabricated run
// id / model on any other bubble (FR-I3).

import { describe, it, expect } from "vitest";
import {
  agentHueMap,
  avatarInitial,
  buildRunComments,
  displayName,
  isRunningState,
} from "@/lib/tickets/runComments";
import type { NormalizedThread } from "@/lib/tickets/thread";

function thread(over: Partial<NormalizedThread>): NormalizedThread {
  return {
    workItemId: "wi-1",
    title: "t",
    description: "",
    state: "in_progress",
    blockedReason: "",
    comments: [],
    changeRefs: [],
    statusHistory: [],
    holder: "",
    runId: "",
    requestedAgent: null,
    assignee: null,
    ...over,
  };
}

describe("avatarInitial", () => {
  it("strips agent:/user: prefixes and upper-cases the first letter", () => {
    expect(avatarInitial("agent:builder")).toBe("B");
    expect(avatarInitial("agent/coder")).toBe("C");
    expect(avatarInitial("user:alice")).toBe("A");
    expect(avatarInitial("")).toBe("?");
  });
});

describe("displayName", () => {
  it("strips the agent:/user: role prefix so the bare name shows (ISI-4567)", () => {
    expect(displayName("agent:Architect")).toBe("Architect");
    expect(displayName("agent/builder")).toBe("builder");
    expect(displayName("user:alice")).toBe("alice");
    expect(displayName("Winston")).toBe("Winston");
    expect(displayName("")).toBe("unknown");
  });

  it("also strips principal:/human: so a person reads as their bare name (ISI-4706)", () => {
    // "flagged under User" / "it should be admin or userx" — never the raw prefix.
    expect(displayName("principal:admin")).toBe("admin");
    expect(displayName("human:henrik")).toBe("henrik");
  });
});

describe("agentHueMap", () => {
  // The property the ticket cares about, asserted directly (not one lucky pair):
  // every distinct agent in a thread gets a DISTINCT hue, up to the palette size.
  // Index assignment guarantees this where a hash could not — a fixed 7-hue hash
  // collided on this repo's own roster (coordinator/tester, architect/coder, …).
  it("spreads the repo's real agent roster across the palette with no collision (ISI-4706)", () => {
    const roster = [
      "architect",
      "builder",
      "coder",
      "coordinator",
      "planner",
      "reviewer",
      "tester",
    ];
    const hues = agentHueMap(roster.map((n) => `agent:${n}`));
    expect(hues.size).toBe(roster.length);
    expect(new Set(hues.values()).size).toBe(roster.length);
  });

  it("assigns by first-appearance index, case/prefix-insensitive, deduped", () => {
    const hues = agentHueMap([
      "agent:Architect",
      "user:alice", // humans skipped
      "agent/architect", // same agent, different prefix/case — no new slot
      "agent:Builder",
    ]);
    expect([...hues.keys()]).toEqual(["architect", "builder"]);
    // First two palette slots, in order — distinct.
    expect(hues.get("architect")).not.toBe(hues.get("builder"));
  });

  it("omits humans so the accent CSS owns them", () => {
    const hues = agentHueMap(["user:alice", "principal:admin", "Winston"]);
    expect(hues.size).toBe(0);
  });

  it("buildRunComments stamps each agent bubble its own hue, humans none", () => {
    const bubbles = buildRunComments(
      thread({
        comments: [
          { author: "agent:builder", body: "wired", createdAt: "2026-09-14T10:00:00Z" },
          { author: "agent:reviewer", body: "lgtm", createdAt: "2026-09-14T10:01:00Z" },
          { author: "user:alice", body: "ok", createdAt: "2026-09-14T10:02:00Z" },
        ],
      }),
    );
    const [builder, reviewer, alice] = bubbles;
    expect(typeof builder.authorHue).toBe("number");
    expect(typeof reviewer.authorHue).toBe("number");
    expect(builder.authorHue).not.toBe(reviewer.authorHue);
    expect(alice.authorHue).toBeUndefined();
  });
});

describe("isRunningState", () => {
  it("is true for in-flight Build/Review phases, false for intake/terminal", () => {
    expect(isRunningState("in_progress")).toBe(true);
    expect(isRunningState("in_review")).toBe(true);
    expect(isRunningState("implementation")).toBe(true);
    expect(isRunningState("code_review")).toBe(true);
    expect(isRunningState("done")).toBe(false);
    expect(isRunningState("cancelled")).toBe(false);
    expect(isRunningState("backlog")).toBe(false);
    expect(isRunningState("todo")).toBe(false);
  });
});

describe("buildRunComments", () => {
  const comments = [
    { author: "agent:builder", body: "first run", createdAt: "2026-09-14T10:00:00Z" },
    { author: "user:alice", body: "looks good", createdAt: "2026-09-14T10:05:00Z" },
    { author: "agent:builder", body: "second run", createdAt: "2026-09-14T10:10:00Z" },
  ];

  it("orders newest last and flags only the newest agent bubble as latest", () => {
    const rc = buildRunComments(thread({ comments, runId: "run-9", state: "in_progress" }));
    expect(rc.map((r) => r.body)).toEqual(["first run", "looks good", "second run"]);
    expect(rc.map((r) => r.isLatestAgent)).toEqual([false, false, true]);
  });

  it("attributes run-id + trace ribbon ONLY to the newest agent bubble", () => {
    const rc = buildRunComments(thread({ comments, runId: "run-9", state: "in_progress" }));
    expect(rc[0].runId).toBeUndefined(); // older agent run: no fabricated id
    expect(rc[1].runId).toBeUndefined(); // human reply: never a run
    expect(rc[2].runId).toBe("run-9");
    expect(rc[2].traceHref).toBe("/runs/run-9");
    expect(rc[0].traceHref).toBeUndefined();
  });

  it("pulses the live dot only when the ticket is in an in-flight phase", () => {
    const live = buildRunComments(thread({ comments, runId: "run-9", state: "in_progress" }));
    expect(live[2].running).toBe(true);
    const done = buildRunComments(thread({ comments, runId: "run-9", state: "done" }));
    expect(done[2].running).toBe(false);
    expect(done[2].runId).toBe("run-9"); // still attributed, just not live
  });

  it("attaches no run meta when the thread carries no holding run", () => {
    const rc = buildRunComments(thread({ comments, runId: "", state: "in_progress" }));
    expect(rc.every((r) => r.runId === undefined)).toBe(true);
    expect(rc.every((r) => r.running === false)).toBe(true);
    // The newest agent bubble is still identifiable for collapse behaviour.
    expect(rc[2].isLatestAgent).toBe(true);
  });

  it("sorts an undated comment to the end (treated as 'just now')", () => {
    const rc = buildRunComments(
      thread({
        comments: [
          { author: "agent:x", body: "dated", createdAt: "2026-09-14T10:00:00Z" },
          { author: "agent:x", body: "undated", createdAt: "" },
        ],
        runId: "run-1",
        state: "in_progress",
      }),
    );
    expect(rc.map((r) => r.body)).toEqual(["dated", "undated"]);
    expect(rc[1].isLatestAgent).toBe(true);
  });
});
