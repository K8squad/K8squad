import { describe, it, expect } from "vitest";
import {
  applyResetMarker,
  applyRoomEvent,
  isResetMarker,
} from "@/lib/discussion/liveFeed";
import { KIND_RESET, type Message } from "@/lib/discussion/types";

// ISI-5674 WS-D — the ADR-0030 clear-signal: a `kind='reset'` marker over the
// live bus clears the visible transcript (everything at/before the floor) and is
// NEVER rendered as a bubble.

function msg(id: string, createdAt: string, kind?: string): Message {
  return {
    id,
    threadId: "thread",
    parentId: null,
    authorPrincipal: kind === KIND_RESET ? "henrik" : "agent-1",
    authorAgentId: kind === KIND_RESET ? null : "agent-1",
    authorRunId: null,
    body: id,
    createdAt,
    kind,
  };
}

describe("isResetMarker", () => {
  it("is true only for kind='reset'", () => {
    expect(isResetMarker(msg("m", "t1", KIND_RESET))).toBe(true);
    expect(isResetMarker(msg("m", "t1"))).toBe(false);
    expect(isResetMarker(msg("m", "t1", "proposal"))).toBe(false);
  });
});

describe("applyResetMarker — floor the transcript", () => {
  it("drops every message at/before the marker AND the marker itself", () => {
    const list = [msg("a", "t1"), msg("b", "t2"), msg("c", "t3")];
    const marker = msg("reset-1", "t4", KIND_RESET);
    expect(applyResetMarker(list, marker)).toEqual([]);
  });

  it("keeps a message created strictly AFTER the marker (post-reset race)", () => {
    const list = [msg("a", "t1"), msg("post", "t9")];
    const marker = msg("reset-1", "t4", KIND_RESET);
    expect(applyResetMarker(list, marker).map((m) => m.id)).toEqual(["post"]);
  });

  it("does not mutate the input list", () => {
    const list = [msg("a", "t1")];
    const copy = [...list];
    applyResetMarker(list, msg("reset-1", "t4", KIND_RESET));
    expect(list).toEqual(copy);
  });
});

describe("applyRoomEvent — reset marker is the clear-signal, never a bubble", () => {
  it("clears the transcript on a reset message.created and omits the marker", () => {
    const s0 = [msg("a", "t1"), msg("b", "t2")];
    const s1 = applyRoomEvent(s0, {
      type: "message.created",
      message: msg("reset-1", "t3", KIND_RESET),
    });
    expect(s1).toEqual([]);
    expect(s1.some((m) => m.id === "reset-1")).toBe(false);
  });

  it("is idempotent — the SSE echo of the same marker is a no-op", () => {
    let s: Message[] = [msg("a", "t1")];
    const marker = msg("reset-1", "t3", KIND_RESET);
    s = applyRoomEvent(s, { type: "message.created", message: marker });
    s = applyRoomEvent(s, { type: "message.created", message: marker });
    expect(s).toEqual([]);
  });

  it("appends an ordinary message posted after a reset", () => {
    let s: Message[] = [msg("a", "t1")];
    s = applyRoomEvent(s, {
      type: "message.created",
      message: msg("reset-1", "t3", KIND_RESET),
    });
    s = applyRoomEvent(s, {
      type: "message.created",
      message: msg("fresh", "t5"),
    });
    expect(s.map((m) => m.id)).toEqual(["fresh"]);
  });
});
