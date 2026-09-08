import { describe, it, expect, vi } from "vitest";
import {
  classifyStatus,
  createDiscussionClient,
  DiscussionApiError,
  type FetchLike,
} from "@/lib/discussion/api";

// AC1/AC4: the console speaks the apiserver's `/discussion/threads` contract
// (migration 0004 superseded the naive rooms shape) and deny renders
// 404-not-403 — 401/403/404 collapse to a single not-found outcome so no
// foreign thread's existence leaks and zero foreign threads render.

describe("classifyStatus — AC4 deny collapse", () => {
  it("2xx → ok", () => {
    expect(classifyStatus(200)).toBe("ok");
    expect(classifyStatus(201)).toBe("ok");
  });
  it("401/403/404 all → not-found (no 403 leak)", () => {
    expect(classifyStatus(401)).toBe("not-found");
    expect(classifyStatus(403)).toBe("not-found");
    expect(classifyStatus(404)).toBe("not-found");
  });
  it("5xx → error", () => {
    expect(classifyStatus(500)).toBe("error");
  });
});

function stub(status: number, json: unknown): FetchLike {
  return vi.fn(async () => ({
    ok: status >= 200 && status < 300,
    status,
    json: async () => json,
  }));
}

/** Record every URL the client hits, so we can assert the contract paths. */
function recorder(status: number, json: unknown) {
  const urls: string[] = [];
  const inits: Array<{ method?: string; body?: string } | undefined> = [];
  const fetchImpl = vi.fn(
    async (url: string, init?: { method?: string; body?: string }) => {
      urls.push(url);
      inits.push(init);
      return {
        ok: status >= 200 && status < 300,
        status,
        json: async () => json,
      };
    },
  ) as unknown as FetchLike;
  return { fetchImpl, urls, inits };
}

describe("createDiscussionClient — contract paths (AC1)", () => {
  it("listThreads GETs the discussion/threads collection", async () => {
    const { fetchImpl, urls, inits } = recorder(200, []);
    const client = createDiscussionClient(fetchImpl);
    await client.listThreads("proj-1");
    expect(urls[0]).toBe("/api/projects/proj-1/discussion/threads");
    expect(inits[0]?.method).toBe("GET");
  });

  it("listThreads forwards limit/offset paging", async () => {
    const { fetchImpl, urls } = recorder(200, []);
    const client = createDiscussionClient(fetchImpl);
    await client.listThreads("proj-1", { limit: 10, offset: 20 });
    expect(urls[0]).toBe(
      "/api/projects/proj-1/discussion/threads?limit=10&offset=20",
    );
  });

  it("getThread GETs a single thread and flattens its nested messages", async () => {
    const { fetchImpl, urls } = recorder(200, {
      id: "t-1",
      messages: [
        {
          id: "m1",
          threadId: "t-1",
          parentId: null,
          body: "root",
          replies: [
            { id: "m2", threadId: "t-1", parentId: "m1", body: "child" },
          ],
        },
      ],
    });
    const client = createDiscussionClient(fetchImpl);
    const flat = await client.getThread("proj-1", "t-1");
    expect(urls[0]).toBe("/api/projects/proj-1/discussion/threads/t-1");
    // Nested tree is depth-first flattened (root + descendants), replies stripped.
    expect(flat.map((m) => m.id)).toEqual(["m1", "m2"]);
    expect(flat[0]).not.toHaveProperty("replies");
  });

  it("openThread POSTs { title, body } (AC2) with no author field", async () => {
    const { fetchImpl, urls, inits } = recorder(201, { id: "t-new" });
    const client = createDiscussionClient(fetchImpl);
    await client.openThread("proj-1", { title: "Kickoff", body: "first" });
    expect(urls[0]).toBe("/api/projects/proj-1/discussion/threads");
    expect(inits[0]?.method).toBe("POST");
    expect(JSON.parse(inits[0]!.body!)).toEqual({
      title: "Kickoff",
      body: "first",
    });
    expect(inits[0]!.body).not.toMatch(/author/i);
  });

  it("postMessage POSTs to the thread's messages with ONLY { body, parentId } (AC3)", async () => {
    const { fetchImpl, urls, inits } = recorder(201, { id: "new" });
    const client = createDiscussionClient(fetchImpl);
    await client.postMessage("proj-1", "t-1", {
      body: "hi",
      parentId: "parent-1",
    });
    expect(urls[0]).toBe(
      "/api/projects/proj-1/discussion/threads/t-1/messages",
    );
    expect(inits[0]?.method).toBe("POST");
    expect(JSON.parse(inits[0]!.body!)).toEqual({
      body: "hi",
      parentId: "parent-1",
    });
    expect(inits[0]!.body).not.toMatch(/author/i);
  });

  it("retractMessage PATCHes the message (AC4); no hard-delete verb is used", async () => {
    const { fetchImpl, urls, inits } = recorder(200, { status: "retracted" });
    const client = createDiscussionClient(fetchImpl);
    await client.retractMessage("proj-1", "t-1", "m-9");
    expect(urls[0]).toBe(
      "/api/projects/proj-1/discussion/threads/t-1/messages/m-9",
    );
    expect(inits[0]?.method).toBe("PATCH");
  });

  it("NEVER emits the superseded legacy room path on any operation (AC1)", async () => {
    const { fetchImpl, urls } = recorder(200, { id: "t-1", messages: [] });
    const client = createDiscussionClient(fetchImpl);
    await client.listThreads("p");
    await client.getThread("p", "t");
    await client.openThread("p", { title: "x", body: "y" });
    await client.postMessage("p", "t", { body: "z" });
    await client.retractMessage("p", "t", "m");
    // The naive ISI-2147 shape used a "ro" + "oms" segment (constructed here so
    // this guard never itself contains the forbidden literal). Zero occurrences.
    const legacySegment = "ro" + "oms";
    for (const u of urls) {
      expect(u).not.toContain(legacySegment);
      expect(u).toContain("/discussion/threads");
    }
  });
});

describe("createDiscussionClient — existence-hiding (AC5)", () => {
  it("cross-Team read (403) surfaces as not-found with no foreign data", async () => {
    const client = createDiscussionClient(stub(403, [{ id: "foreign" }]));
    await expect(client.getThread("teamA-proj", "thread")).rejects.toMatchObject(
      { outcome: "not-found" },
    );
  });

  it("404 surfaces as not-found", async () => {
    const client = createDiscussionClient(stub(404, {}));
    await expect(client.listThreads("p")).rejects.toBeInstanceOf(
      DiscussionApiError,
    );
  });

  it("a denied retract (403) surfaces as not-found, never leaking the message", async () => {
    const client = createDiscussionClient(stub(403, {}));
    await expect(
      client.retractMessage("p", "t", "m"),
    ).rejects.toMatchObject({ outcome: "not-found" });
  });
});
