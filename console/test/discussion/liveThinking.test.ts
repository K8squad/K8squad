import { describe, it, expect } from "vitest";
import {
  authorAgentName,
  correlateThinking,
  appendThinking,
} from "@/lib/discussion/liveThinking";
import type { DispatchWatch } from "@/lib/discussion/working";
import type { ThinkingRow } from "@/lib/discussion/liveFeed";

function watch(
  messageId: string,
  agentName: string,
  phase: DispatchWatch["phase"] = "working",
  since = 1000,
): DispatchWatch {
  return { key: `${messageId}:${agentName.toLowerCase()}`, agentName, messageId, phase, since };
}

function row(author: string, body = "…", runId = "r1", seq = 1): ThinkingRow {
  return { runId, author, body, seq, at: "2026-09-29T10:00:00Z" };
}

describe("liveThinking — author prefix", () => {
  it("strips the agent: prefix; leaves run/ and bare names as-is", () => {
    expect(authorAgentName("agent:Robo-Coder")).toBe("Robo-Coder");
    expect(authorAgentName("run/ab12cd34")).toBe("run/ab12cd34");
    expect(authorAgentName("john")).toBe("john");
    expect(authorAgentName("")).toBe("");
  });
});

describe("liveThinking — correlateThinking (ISI-5208)", () => {
  it("matches a working watch by agent name (case-insensitive, agent: prefix)", () => {
    const w = [watch("m1", "Robo-Coder")];
    expect(correlateThinking(w, "agent:robo-coder")).toBe("m1");
  });

  it("falls back to the lone active watch when the author matches no name", () => {
    const w = [watch("m1", "john")];
    // A run-stamped fallback author (assignee unresolved) still lands under the
    // single active dispatch.
    expect(correlateThinking(w, "run/ab12cd34")).toBe("m1");
  });

  it("drops (null) when the author matches nothing and several are active (ambiguous)", () => {
    const w = [watch("m1", "john"), watch("m2", "mary")];
    expect(correlateThinking(w, "run/zz")).toBeNull();
  });

  it("prefers the most recently seeded watch when several share the agent name", () => {
    const w = [watch("m1", "john", "working", 1000), watch("m2", "john", "working", 2000)];
    expect(correlateThinking(w, "agent:john")).toBe("m2");
  });

  it("ignores non-working watches (replied/failed never receive thinking)", () => {
    const w = [watch("m1", "john", "replied"), watch("m2", "john", "failed")];
    expect(correlateThinking(w, "agent:john")).toBeNull();
  });

  it("returns null when there are no watches at all", () => {
    expect(correlateThinking([], "agent:john")).toBeNull();
  });
});

describe("liveThinking — appendThinking reducer", () => {
  it("appends a correlated row under its triggering message", () => {
    const w = [watch("m1", "john")];
    const out = appendThinking({}, row("agent:john", "reading thread"), w);
    expect(out.m1.map((r) => r.body)).toEqual(["reading thread"]);
  });

  it("keeps arrival order across multiple rows for one message", () => {
    const w = [watch("m1", "john")];
    let s: Record<string, ThinkingRow[]> = {};
    s = appendThinking(s, row("agent:john", "a", "r1", 1), w);
    s = appendThinking(s, row("agent:john", "b", "r1", 2), w);
    expect(s.m1.map((r) => r.body)).toEqual(["a", "b"]);
  });

  it("de-dupes a replayed row by (runId, seq)", () => {
    const w = [watch("m1", "john")];
    let s: Record<string, ThinkingRow[]> = {};
    s = appendThinking(s, row("agent:john", "a", "r1", 1), w);
    s = appendThinking(s, row("agent:john", "a", "r1", 1), w); // SSE replay
    expect(s.m1).toHaveLength(1);
  });

  it("drops an uncorrelated row (no active dispatch it can belong to)", () => {
    const w = [watch("m1", "john"), watch("m2", "mary")];
    const out = appendThinking({}, row("run/other"), w);
    expect(out).toEqual({});
  });

  it("does not mutate the input map", () => {
    const w = [watch("m1", "john")];
    const input = Object.freeze({});
    expect(() => appendThinking(input, row("agent:john"), w)).not.toThrow();
  });
});
