// test/tickets/api.test.ts — the BFF read contract for the Tickets screen
// (ISI-4132): the M1.5 board read model (ISI-4131) answers a BARE JSON array,
// which the 8.14d-era { items } unwrap silently dropped ("No tickets in this
// Project" with a full board upstream).

import { describe, it, expect, vi, beforeEach } from "vitest";

const fetchMock = vi.fn();
vi.stubGlobal("fetch", fetchMock);

import { listWorkItems } from "@/lib/tickets/api";

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json" },
  });
}

describe("listWorkItems payload shapes", () => {
  beforeEach(() => fetchMock.mockReset());

  it("accepts the M1.5 bare array", async () => {
    fetchMock.mockResolvedValue(
      jsonResponse([{ id: "wi-1", title: "ship", state: "todo" }]),
    );
    const items = await listWorkItems("bmad-squad/bmad-demo-project");
    expect(items).toHaveLength(1);
    expect(items[0].id).toBe("wi-1");
  });

  it("keeps the legacy { items } envelope working", async () => {
    fetchMock.mockResolvedValue(
      jsonResponse({ items: [{ id: "wi-2", title: "legacy", state: "done" }] }),
    );
    const items = await listWorkItems("p");
    expect(items).toHaveLength(1);
    expect(items[0].id).toBe("wi-2");
  });

  it("renders an empty board for an empty array (never null upstream)", async () => {
    fetchMock.mockResolvedValue(jsonResponse([]));
    expect(await listWorkItems("p")).toEqual([]);
  });
});
