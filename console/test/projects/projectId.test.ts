// ISI-3982 — the Project id ("namespace/name") must survive the round-trip from a
// Projects-list row, through the [projectId] page/redirect, the browser client, and the
// BFF proxy, arriving at the Go apiserver encoded EXACTLY ONCE ("ns%2Fname"). The bug
// was a re-encode at every hop that triple-mangled the id into a mux 404. These tests
// pin the single-encoding invariant at the two boundaries that enforce it (the pure
// normalizers) and at the BFF routes that were the last re-encode before the apiserver.

import { describe, it, expect, vi, beforeEach } from "vitest";
import { decodeProjectId, encodeProjectId } from "@/lib/projectId";

const NS_NAME = "bmad-squad/bmad-demo-project";
const ENCODED = "bmad-squad%2Fbmad-demo-project"; // single layer — what the apiserver mux wants

describe("projectId normalizers (ISI-3982)", () => {
  it("decodeProjectId returns the canonical ns/name from the encoded segment", () => {
    expect(decodeProjectId(ENCODED)).toBe(NS_NAME);
  });

  it("decodeProjectId is a no-op on an already-decoded id (idempotent for our shape)", () => {
    expect(decodeProjectId(NS_NAME)).toBe(NS_NAME);
    expect(decodeProjectId(decodeProjectId(ENCODED))).toBe(NS_NAME);
  });

  it("decodeProjectId passes a malformed escape through rather than throwing", () => {
    expect(() => decodeProjectId("100%-done")).not.toThrow();
    expect(decodeProjectId("100%-done")).toBe("100%-done");
  });

  it("encodeProjectId yields exactly ONE encoding layer from a decoded id", () => {
    expect(encodeProjectId(NS_NAME)).toBe(ENCODED);
  });

  it("encodeProjectId collapses an already-encoded segment back to one layer (no double-encode)", () => {
    // This is the core fix: re-normalizing the router-preserved "ns%2Fname" must NOT
    // produce "ns%252Fname".
    expect(encodeProjectId(ENCODED)).toBe(ENCODED);
    expect(encodeProjectId(ENCODED)).not.toContain("%252F");
  });

  it("encodeProjectId is idempotent — normalize(normalize(x)) === normalize(x)", () => {
    for (const x of [NS_NAME, ENCODED]) {
      expect(encodeProjectId(encodeProjectId(x))).toBe(encodeProjectId(x));
    }
  });

  it("full row→page→client→BFF chain converges on a single-encoded id", () => {
    // 1. Projects-list row builds the link from the decoded id.
    const rowSegment = encodeProjectId(NS_NAME); // "ns%2Fname"
    // 2. The [projectId] page decodes the router-preserved segment for the screen.
    const forScreen = decodeProjectId(rowSegment); // "ns/name"
    // 3. The browser client re-encodes exactly once when it calls the BFF.
    const clientSegment = encodeProjectId(forScreen); // "ns%2Fname"
    // 4. The BFF route normalizes the router-preserved segment for the apiserver.
    const upstream = encodeProjectId(clientSegment); // "ns%2Fname"
    expect(upstream).toBe(ENCODED);
    expect(upstream).not.toContain("%252F");
  });
});

// The BFF routes were the LAST re-encode before the apiserver (route.ts step 4 of the
// root-cause chain). Mock the proxy layer and assert the upstream path each route builds
// is single-encoded regardless of whether the router handed it the encoded or decoded
// segment. Mocking @/lib/bff also keeps the real (server-only, network) proxy out of the
// unit run.
const proxyJson = vi.fn(async () => new Response(null, { status: 200 }));
const proxyJsonWrite = vi.fn(async () => new Response(null, { status: 201 }));
const proxyEventStream = vi.fn(async () => new Response(null, { status: 200 }));
vi.mock("@/lib/bff", () => ({
  proxyJson: (...a: unknown[]) => proxyJson(...(a as [])),
  proxyJsonWrite: (...a: unknown[]) => proxyJsonWrite(...(a as [])),
  proxyEventStream: (...a: unknown[]) => proxyEventStream(...(a as [])),
}));

import { GET as workItemsGET, POST as workItemsPOST } from "@/app/api/projects/[projectId]/work-items/route";
import { GET as roomsGET } from "@/app/api/projects/[projectId]/rooms/route";
import { GET as streamGET } from "@/app/api/projects/[projectId]/stream/route";
import { GET as messagesGET } from "@/app/api/projects/[projectId]/rooms/[roomId]/messages/route";

// Minimal NextRequest stand-in — the routes read only `nextUrl.search`.
function fakeReq(search = ""): import("next/server").NextRequest {
  return { nextUrl: { search } } as unknown as import("next/server").NextRequest;
}

/** The upstream path passed to the proxy (2nd positional arg). */
function pathOf(spy: ReturnType<typeof vi.fn>): string {
  const call = spy.mock.calls.at(-1) as unknown[];
  return call[1] as string;
}

describe("Project BFF routes forward a single-encoded id (ISI-3982)", () => {
  beforeEach(() => {
    proxyJson.mockClear();
    proxyJsonWrite.mockClear();
    proxyEventStream.mockClear();
  });

  // The router hands route handlers the still-encoded segment for ids with an encoded
  // slash; the client encodes exactly once, so a route sees EITHER the encoded segment
  // or (if the router decoded it) the plain id. Feed both and require the same
  // single-encoded upstream path. (A double-encoded segment is the OLD defect OUTPUT,
  // never a post-fix route INPUT — the client no longer emits one.)
  for (const [shape, param] of [
    ["router-encoded", ENCODED],
    ["decoded", NS_NAME],
  ] as const) {
    it(`work-items GET forwards ns%2Fname once (${shape})`, async () => {
      await workItemsGET(fakeReq("?parentId=x"), {
        params: Promise.resolve({ projectId: param }),
      });
      const p = pathOf(proxyJson);
      expect(p).toBe(`/api/projects/${ENCODED}/work-items?parentId=x`);
      expect(p).not.toContain("%252F");
    });
  }

  it("work-items POST forwards ns%2Fname once", async () => {
    await workItemsPOST(fakeReq(), {
      params: Promise.resolve({ projectId: ENCODED }),
    });
    expect(pathOf(proxyJsonWrite)).toBe(`/api/projects/${ENCODED}/work-items`);
  });

  it("rooms GET forwards ns%2Fname once", async () => {
    await roomsGET(fakeReq(), { params: Promise.resolve({ projectId: ENCODED }) });
    const p = pathOf(proxyJson);
    expect(p).toBe(`/api/projects/${ENCODED}/rooms`);
    expect(p).not.toContain("%252F");
  });

  it("stream GET forwards ns%2Fname once", async () => {
    await streamGET(fakeReq(), { params: Promise.resolve({ projectId: ENCODED }) });
    expect(pathOf(proxyEventStream)).toBe(`/api/projects/${ENCODED}/stream`);
  });

  it("room messages GET forwards ns%2Fname once and preserves the query", async () => {
    await messagesGET(fakeReq("?threadDepth=100"), {
      params: Promise.resolve({ projectId: ENCODED, roomId: "room-1" }),
    });
    const p = pathOf(proxyJson);
    expect(p).toBe(`/api/projects/${ENCODED}/rooms/room-1/messages?threadDepth=100`);
    expect(p).not.toContain("%252F");
  });
});
