// test/tickets/thread.postComment.test.ts — the ticket-detail comment sender's
// opt-in Idempotency-Key relay (ISI-7907). A stable per-submit token rides as the
// Idempotency-Key header (never the wire body) so the apiserver's short-window
// dedup collapses a double-clicked or retried POST to one insert; omitting it keeps
// the exact { body } wire the comment always sent.

import { describe, it, expect, vi, beforeEach } from "vitest";

const fetchMock = vi.fn();
vi.stubGlobal("fetch", fetchMock);

import { postWorkItemComment } from "@/lib/tickets/thread";

function jsonResponse(body: unknown, status = 201): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json" },
  });
}

/** The init.headers the sender actually sent, normalized to a plain record. */
function sentHeaders(): Record<string, string> {
  const init = fetchMock.mock.calls[0][1] as {
    headers?: Record<string, string>;
  };
  return init.headers ?? {};
}

describe("postWorkItemComment — Idempotency-Key (ISI-7907)", () => {
  beforeEach(() => fetchMock.mockReset());

  it("sends the Idempotency-Key header when a key is supplied", async () => {
    fetchMock.mockResolvedValue(
      jsonResponse({ author: "a", body: "b", createdAt: "t" }),
    );
    await postWorkItemComment("wi-1", "hello", undefined, "key-xyz");
    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(fetchMock.mock.calls[0][0]).toBe("/api/work-items/wi-1/comments");
    expect(sentHeaders()["idempotency-key"]).toBe("key-xyz");
  });

  it("omits the Idempotency-Key header and keeps the { body } wire when no key is given", async () => {
    fetchMock.mockResolvedValue(
      jsonResponse({ author: "a", body: "b", createdAt: "t" }),
    );
    await postWorkItemComment("wi-1", "hello");
    const init = fetchMock.mock.calls[0][1] as {
      headers?: Record<string, string>;
      body?: string;
    };
    expect(init.headers).not.toHaveProperty("idempotency-key");
    expect(JSON.parse(init.body ?? "{}")).toEqual({ body: "hello" });
  });
});
