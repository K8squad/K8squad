import { describe, it, expect } from "vitest";
import { buildPostBody, canSubmit } from "@/lib/discussion/compose";

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
    const out = buildPostBody({
      body: "x",
      parentId: "p",
    }) as unknown as Record<string, unknown>;
    for (const k of FORBIDDEN) expect(out).not.toHaveProperty(k);
  });

  it("trims body and drops a blank parentId", () => {
    const out = buildPostBody({ body: "  spaced  ", parentId: "   " });
    expect(out.body).toBe("spaced");
    expect(out).not.toHaveProperty("parentId");
  });

  it("serialized wire form carries no author key", () => {
    const wire = JSON.stringify(
      buildPostBody({ body: "audit me", parentId: "p9" }),
    );
    expect(wire).not.toMatch(/author/i);
    expect(JSON.parse(wire)).toEqual({ body: "audit me", parentId: "p9" });
  });
});

describe("buildPostBody — audience (ISI-4929, plan §4.1/§4.2)", () => {
  it("party default keeps the body minimal — no audience key", () => {
    const out = buildPostBody({ body: "hi", audience: { kind: "party" } });
    expect(Object.keys(out).sort()).toEqual(["body"]);
  });

  it("a direct audience emits audience: direct:{agentId}", () => {
    const out = buildPostBody({
      body: "psst",
      audience: { kind: "direct", agentId: "agent-7" },
    });
    expect(out).toEqual({ body: "psst", audience: "direct:agent-7" });
  });

  it("is idempotent over an already-built wire body (double-build is a no-op)", () => {
    const once = buildPostBody({
      body: "b",
      audience: { kind: "direct", agentId: "a-1" },
    });
    expect(buildPostBody(once)).toEqual(once);
  });

  it("audience never widens the wire with author fields", () => {
    const out = buildPostBody({
      body: "x",
      audience: { kind: "direct", agentId: "a" },
    }) as unknown as Record<string, unknown>;
    for (const k of FORBIDDEN) expect(out).not.toHaveProperty(k);
  });
});

describe("buildPostBody — ticket references (ISI-5167 / ISI-5134 S3)", () => {
  it("no references ⇒ no `references` key (a plain post stays link-free)", () => {
    expect(buildPostBody({ body: "hi" })).not.toHaveProperty("references");
    expect(buildPostBody({ body: "hi", references: [] })).not.toHaveProperty(
      "references",
    );
  });

  it("collects picked refs into the wire body as { workItemId, title }", () => {
    const out = buildPostBody({
      body: "see #Fix flaky test",
      references: [{ workItemId: "wi-1", title: "Fix flaky test" }],
    });
    expect(out).toEqual({
      body: "see #Fix flaky test",
      references: [{ workItemId: "wi-1", title: "Fix flaky test" }],
    });
  });

  it("de-dupes by work-item id and drops blank ids", () => {
    const out = buildPostBody({
      body: "x",
      references: [
        { workItemId: "wi-1", title: "One" },
        { workItemId: "wi-1", title: "One again" },
        { workItemId: "  ", title: "blank" },
        { workItemId: "wi-2" },
      ],
    });
    expect(out.references).toEqual([
      { workItemId: "wi-1", title: "One" },
      { workItemId: "wi-2" },
    ]);
  });

  it("references ride ALONGSIDE audience without dropping either", () => {
    const out = buildPostBody({
      body: "psst",
      audience: { kind: "direct", agentId: "agent-7" },
      references: [{ workItemId: "wi-9", title: "Ship it" }],
    });
    expect(out).toEqual({
      body: "psst",
      audience: "direct:agent-7",
      references: [{ workItemId: "wi-9", title: "Ship it" }],
    });
  });

  it("is idempotent over an already-built body carrying references", () => {
    const once = buildPostBody({
      body: "b",
      references: [{ workItemId: "wi-1", title: "One" }],
    });
    expect(buildPostBody(once)).toEqual(once);
  });

  it("references never widen the wire with author fields", () => {
    const out = buildPostBody({
      body: "x",
      references: [{ workItemId: "wi-1" }],
    }) as unknown as Record<string, unknown>;
    for (const k of FORBIDDEN) expect(out).not.toHaveProperty(k);
  });
});

describe("canSubmit", () => {
  it("rejects empty/whitespace bodies", () => {
    expect(canSubmit({ body: "" })).toBe(false);
    expect(canSubmit({ body: "   " })).toBe(false);
    expect(canSubmit({ body: "ok" })).toBe(true);
  });
});
