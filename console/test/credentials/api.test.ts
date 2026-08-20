import { describe, it, expect, vi, afterEach } from "vitest";
import { NextRequest } from "next/server";
import { GET as credentialsGET } from "@/app/api/credentials/route";
import { POST as connectPOST } from "@/app/api/credentials/connect/route";

// The 8.6 BFF routes at the proxy boundary: the session cookie rides upstream,
// the apiserver's status surfaces VERBATIM (501 stays 501 — the documented
// unconfigured contract; a deny stays itself), and no write verb is routable on
// the read path.

const realFetch = globalThis.fetch;

function stubFetch(status: number, body: unknown) {
  return vi.fn(async () => jsonResponse(status, body));
}

function jsonResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json" },
  });
}

function makeReq(path: string, init?: RequestInit): NextRequest {
  // Next's RequestInit narrows signal to non-null; the DOM-flavoured RequestInit the test builds
  // is cast through unknown into the constructor's own parameter type (the handler only reads
  // headers/method from it).
  return new NextRequest(
    `http://console.local${path}`,
    init as unknown as ConstructorParameters<typeof NextRequest>[1],
  );
}

afterEach(() => {
  globalThis.fetch = realFetch;
  vi.restoreAllMocks();
});

describe("GET /api/credentials — the read proxy", () => {
  it("forwards the session cookie upstream and relays status + body verbatim", async () => {
    const fetchMock = stubFetch(200, { team: "squad-a", agents: [] });
    globalThis.fetch = fetchMock as unknown as typeof fetch;

    const res = await credentialsGET(
      makeReq("/api/credentials", { headers: { cookie: "ksquad_session=dev-token-abc" } }),
    );

    expect(res.status).toBe(200);
    const sent = (fetchMock.mock.calls[0] as unknown[])[0] as string;
    expect(sent).toContain("/api/credentials");
    const init = (fetchMock.mock.calls[0] as unknown[])[1] as { headers: Headers };
    expect(init.headers.get("cookie")).toBe("ksquad_session=dev-token-abc");
    expect(await res.json()).toEqual({ team: "squad-a", agents: [] });
  });

  it("relays the documented 501 VERBATIM (unconfigured contract, never re-mapped)", async () => {
    globalThis.fetch = stubFetch(501, { error: "not implemented", tracking: "ISI-2902" }) as unknown as typeof fetch;
    const res = await credentialsGET(makeReq("/api/credentials"));
    expect(res.status).toBe(501);
    expect((await res.json()).tracking).toBe("ISI-2902");
  });

  it("relays a deny (403) as 403 — the apiserver's decision is the authority", async () => {
    globalThis.fetch = stubFetch(403, { error: "denied" }) as unknown as typeof fetch;
    const res = await credentialsGET(makeReq("/api/credentials"));
    expect(res.status).toBe(403);
  });
});

describe("POST /api/credentials/connect — the 7.7 seam proxy", () => {
  it("POSTs upstream with the caller's cookie and relays the 501 naming ISI-2899", async () => {
    const fetchMock = stubFetch(501, { error: "not implemented", tracking: "ISI-2899: credential controller + Connect Claude OAuth flow" });
    globalThis.fetch = fetchMock as unknown as typeof fetch;

    const res = await connectPOST(
      makeReq("/api/credentials/connect", {
        method: "POST",
        headers: { cookie: "ksquad_session=dev-token-abc" },
      }),
    );

    expect(res.status).toBe(501);
    const init = (fetchMock.mock.calls[0] as unknown[])[1] as { method: string; headers: Headers };
    expect(init.method).toBe("POST");
    expect(init.headers.get("cookie")).toBe("ksquad_session=dev-token-abc");
    expect((await res.json()).tracking).toContain("ISI-2899");
  });

  it("relays a 200 (flow started) verbatim once 7.7 lands", async () => {
    globalThis.fetch = stubFetch(200, { loginUrl: "https://auth.example/start" }) as unknown as typeof fetch;
    const res = await connectPOST(makeReq("/api/credentials/connect", { method: "POST" }));
    expect(res.status).toBe(200);
  });
});
