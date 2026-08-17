import { describe, it, expect } from "vitest";
import { buildPostBody, canSubmit } from "../src/lib/discussion/compose";

// AC3 (server-stamp boundary): the outbound body is ONLY { body, parentId? }.
// A console that sends any author field is a defect.

const FORBIDDEN = [
  "author",
  "authorId",
  "authorType",
  "authorName",
  "author_agent_id",
  "author_run_id",
  "principal",
];

describe("buildPostBody — AC3 server-stamp boundary", () => {
  it("new top-level message → { body } only", () => {
    const out = buildPostBody({ body: "hello room" });
    expect(Object.keys(out).sort()).toEqual(["body"]);
    expect(out.body).toBe("hello room");
  });

  it("reply-in-thread → { body, parentId } only", () => {
    const out = buildPostBody({ body: "re: hi", parentId: "p-1" });
    expect(Object.keys(out).sort()).toEqual(["body", "parentId"]);
    expect(out.parentId).toBe("p-1");
  });

  it("NEVER emits any author/provenance field", () => {
    const out = buildPostBody({ body: "x", parentId: "p" }) as unknown as Record<string, unknown>;
    for (const k of FORBIDDEN) expect(out).not.toHaveProperty(k);
  });

  it("trims body and drops a blank parentId", () => {
    const out = buildPostBody({ body: "  spaced  ", parentId: "   " });
    expect(out.body).toBe("spaced");
    expect(out).not.toHaveProperty("parentId");
  });

  it("serialized wire form carries no author key", () => {
    const wire = JSON.stringify(buildPostBody({ body: "audit me", parentId: "p9" }));
    expect(wire).not.toMatch(/author/i);
    expect(JSON.parse(wire)).toEqual({ body: "audit me", parentId: "p9" });
  });
});

describe("canSubmit", () => {
  it("rejects empty/whitespace bodies", () => {
    expect(canSubmit({ body: "" })).toBe(false);
    expect(canSubmit({ body: "   " })).toBe(false);
    expect(canSubmit({ body: "ok" })).toBe(true);
  });
});
