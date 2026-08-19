import { describe, it, expect, vi } from "vitest";
import {
  classifyStatus,
  createDiscussionClient,
  DiscussionApiError,
  type FetchLike,
} from "@/lib/discussion/api";

// AC4: deny renders 404-not-403; 401/403/404 collapse to a single not-found
// outcome so no foreign room's existence leaks and zero foreign threads render.

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

describe("createDiscussionClient", () => {
  it("cross-Team read (403) surfaces as not-found with no foreign data", async () => {
    const client = createDiscussionClient(stub(403, [{ id: "foreign" }]));
    await expect(
      client.getMessages("teamA-proj", "room"),
    ).rejects.toMatchObject({
      outcome: "not-found",
    });
  });

  it("404 surfaces as not-found", async () => {
    const client = createDiscussionClient(stub(404, {}));
    await expect(client.listRooms("p")).rejects.toBeInstanceOf(
      DiscussionApiError,
    );
  });

  it("postMessage sends ONLY { body, parentId } on the wire (AC3 at the client)", async () => {
    const fetchImpl = vi.fn(async () => ({
      ok: true,
      status: 201,
      json: async () => ({ id: "new" }),
    }));
    const client = createDiscussionClient(fetchImpl as unknown as FetchLike);
    await client.postMessage("p", "room", {
      body: "hi",
      parentId: "parent-1",
    });
    const call = fetchImpl.mock.calls[0] as unknown as unknown[];
    const init = call[1] as { method: string; body: string };
    expect(init.method).toBe("POST");
    expect(JSON.parse(init.body)).toEqual({ body: "hi", parentId: "parent-1" });
    expect(init.body).not.toMatch(/author/i);
  });

  it("GET messages builds the BFF path with thread depth", async () => {
    const fetchImpl = vi.fn(async () => ({
      ok: true,
      status: 200,
      json: async () => [],
    }));
    const client = createDiscussionClient(fetchImpl as unknown as FetchLike);
    await client.getMessages("proj-1", "room-1", { threadDepth: 100 });
    const call = fetchImpl.mock.calls[0] as unknown as unknown[];
    expect(call[0]).toBe(
      "/api/projects/proj-1/rooms/room-1/messages?threadDepth=100",
    );
  });
});
