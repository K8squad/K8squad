import { describe, it, expect } from "vitest";
import {
  applyRoomEvent,
  upsertMessage,
  parseRoomEvent,
} from "../src/lib/discussion/liveFeed";
import type { Message } from "../src/lib/discussion/types";

function msg(id: string, createdAt: string, body = id): Message {
  return {
    id,
    roomId: "room",
    parentId: null,
    authorId: "a",
    authorType: "agent",
    authorName: "agent-1",
    body,
    kind: "message",
    createdAt,
  };
}

// AC6: live append over the single 8.2 SSE channel — idempotent by id.

describe("liveFeed — AC6 live append", () => {
  it("appends a created message in createdAt order", () => {
    const s0: Message[] = [msg("a", "t1")];
    const s1 = applyRoomEvent(s0, { type: "message.created", message: msg("b", "t2") });
    expect(s1.map((m) => m.id)).toEqual(["a", "b"]);
  });

  it("dedupes the server echo of a just-posted message (no duplicate row)", () => {
    let s: Message[] = [];
    const m = msg("x", "t1");
    s = applyRoomEvent(s, { type: "message.created", message: m }); // optimistic
    s = applyRoomEvent(s, { type: "message.created", message: m }); // SSE echo
    expect(s.filter((r) => r.id === "x")).toHaveLength(1);
  });

  it("updated replaces the existing row in place", () => {
    const s0 = [msg("x", "t1", "old")];
    const s1 = applyRoomEvent(s0, {
      type: "message.updated",
      message: msg("x", "t1", "new"),
    });
    expect(s1).toHaveLength(1);
    expect(s1[0].body).toBe("new");
  });

  it("deleted removes the row", () => {
    const s0 = [msg("x", "t1"), msg("y", "t2")];
    const s1 = applyRoomEvent(s0, { type: "message.deleted", id: "x" });
    expect(s1.map((m) => m.id)).toEqual(["y"]);
  });

  it("upsert does not mutate the input list", () => {
    const s0 = [msg("a", "t1")];
    upsertMessage(s0, msg("b", "t2"));
    expect(s0).toHaveLength(1);
  });
});

describe("parseRoomEvent", () => {
  it("parses created/updated/deleted envelopes", () => {
    expect(parseRoomEvent(JSON.stringify({ type: "message.deleted", id: "z" }))).toEqual({
      type: "message.deleted",
      id: "z",
    });
    const created = parseRoomEvent(
      JSON.stringify({ type: "message.created", message: msg("m", "t1") }),
    );
    expect(created?.type).toBe("message.created");
  });
  it("returns null on garbage / non-events", () => {
    expect(parseRoomEvent("not json")).toBeNull();
    expect(parseRoomEvent(JSON.stringify({ type: "other" }))).toBeNull();
    expect(parseRoomEvent(JSON.stringify({ type: "message.created" }))).toBeNull();
  });
});
