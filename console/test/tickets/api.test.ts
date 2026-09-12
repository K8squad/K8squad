// test/tickets/api.test.ts — the BFF read contract for the Tickets screen
// (ISI-4132): the M1.5 board read model (ISI-4131) answers a BARE JSON array,
// which the 8.14d-era { items } unwrap silently dropped ("No tickets in this
// Project" with a full board upstream).
//
// Also pins the state-transition WRITE body (ISI-4225): the console client must
// serialize the apiserver's field names {toState, fromState} — byte-for-byte the
// shared fixture, which the Go handler contract test
// (internal/apiserver/workitemstate_contract_test.go) replays through the real
// handler so neither tree can drift from the other silently.

import { describe, it, expect, vi, beforeEach } from "vitest";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";

const fetchMock = vi.fn();
vi.stubGlobal("fetch", fetchMock);

import { listWorkItems, patchWorkItemState } from "@/lib/tickets/api";

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

// The single source of truth for the cross-tree request-body contract: this
// exact file is POSTed to the apiserver handler by the Go contract test.
// (cwd-relative — under jsdom, import.meta.url is not a file: URL.)
const contractBody = readFileSync(
  resolve(process.cwd(), "test/tickets/fixtures/state-transition-request.json"),
  "utf8",
).trim();

describe("patchWorkItemState request contract (ISI-4225)", () => {
  beforeEach(() => fetchMock.mockReset());

  it("serializes exactly the fixture body the apiserver handler decodes", async () => {
    fetchMock.mockResolvedValue(
      jsonResponse({
        workItemId: "wi-1",
        fromState: "todo",
        toState: "in_progress",
      }),
    );
    await patchWorkItemState("wi-1", {
      toState: "in_progress",
      fromState: "todo",
    });

    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [url, init] = fetchMock.mock.calls[0] as [
      string,
      RequestInit & { headers: Record<string, string> },
    ];
    expect(url).toBe("/api/work-items/wi-1/state");
    expect(init.method).toBe("PATCH");
    expect(init.headers["content-type"]).toBe("application/json");
    // EXACT string equality — key names AND order — against the shared fixture.
    // The 8.14a-era client spoke {to, expectedFrom} here and every browser lane
    // move 400'd ("toState required") because the BFF forwards verbatim.
    expect(init.body).toBe(contractBody);
  });

  it("resolves the target state on 200", async () => {
    fetchMock.mockResolvedValue(
      jsonResponse({
        workItemId: "wi-1",
        fromState: "todo",
        toState: "in_progress",
      }),
    );
    const out = await patchWorkItemState("wi-1", {
      toState: "in_progress",
      fromState: "todo",
    });
    expect(out).toEqual({ state: "in_progress" });
  });

  it("rethrows non-2xx as ApiError so the screen re-syncs (never keeps client state)", async () => {
    fetchMock.mockResolvedValue(jsonResponse({ error: "toState required" }, 400));
    await expect(
      patchWorkItemState("wi-1", {
        toState: "in_progress",
        fromState: "todo",
      }),
    ).rejects.toMatchObject({ status: 400 });
  });
});
