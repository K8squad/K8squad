import { describe, it, expect, vi, afterEach } from "vitest";
import { NextRequest } from "next/server";
import { POST as sessionPOST, DELETE as sessionDELETE } from "@/app/api/session/route";

// ISI-3522: the /login flow's BFF leg. The console form POSTs {username,password} to /api/session,
// which proxies apiserver POST /auth/login (the ONE authz choke point, §13) and — critically —
// RELAYS the apiserver's Set-Cookie so the HttpOnly ksquad_session actually lands in the browser.
// Status is surfaced verbatim (401 invalid creds, 429 rate-limited). Logout relays the clearing cookie.

const realFetch = globalThis.fetch;

function makeReq(path: string, init?: RequestInit): NextRequest {
  return new NextRequest(
    `http://console.local${path}`,
    init as unknown as ConstructorParameters<typeof NextRequest>[1],
  );
}

afterEach(() => {
  globalThis.fetch = realFetch;
  vi.restoreAllMocks();
});

describe("POST /api/session — the login proxy", () => {
  it("forwards credentials upstream and RELAYS the session Set-Cookie to the browser", async () => {
    const upstream = new Response(JSON.stringify({ user: { id: "u1" } }), {
      status: 200,
      headers: {
        "content-type": "application/json",
        "set-cookie": "ksquad_session=opaque-token; Path=/; HttpOnly; SameSite=Lax",
      },
    });
    const fetchMock = vi.fn(async () => upstream);
    globalThis.fetch = fetchMock as unknown as typeof fetch;

    const res = await sessionPOST(
      makeReq("/api/session", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ username: "op", password: "pw" }),
      }),
    );

    // Upstream target is the apiserver login route, and the body was forwarded unchanged.
    const sentUrl = (fetchMock.mock.calls[0] as unknown[])[0] as string;
    expect(sentUrl).toContain("/auth/login");
    const init = (fetchMock.mock.calls[0] as unknown[])[1] as { method: string; body?: string };
    expect(init.method).toBe("POST");
    expect(init.body).toBe(JSON.stringify({ username: "op", password: "pw" }));

    // The cookie the apiserver minted is relayed back — without this, login silently no-ops.
    expect(res.status).toBe(200);
    expect(res.headers.get("set-cookie")).toContain("ksquad_session=opaque-token");
    expect(res.headers.get("set-cookie")).toContain("HttpOnly");
  });

  it("surfaces the apiserver's opaque 401 verbatim (no cookie leaks on a failed login)", async () => {
    globalThis.fetch = (vi.fn(async () =>
      new Response(JSON.stringify({ error: "invalid credentials" }), {
        status: 401,
        headers: { "content-type": "application/json" },
      }),
    ) as unknown) as typeof fetch;

    const res = await sessionPOST(
      makeReq("/api/session", { method: "POST", body: JSON.stringify({ username: "x", password: "y" }) }),
    );
    expect(res.status).toBe(401);
    expect(res.headers.get("set-cookie")).toBeNull();
  });
});

describe("DELETE /api/session — the logout proxy", () => {
  it("proxies /auth/logout and relays the cookie-clearing Set-Cookie", async () => {
    const fetchMock = vi.fn(async () =>
      new Response(null, {
        status: 204,
        headers: { "set-cookie": "ksquad_session=; Path=/; Max-Age=0; HttpOnly" },
      }),
    );
    globalThis.fetch = fetchMock as unknown as typeof fetch;

    const res = await sessionDELETE(makeReq("/api/session", { method: "DELETE" }));

    expect((fetchMock.mock.calls[0] as unknown[])[0]).toContain("/auth/logout");
    expect(res.status).toBe(204);
    expect(res.headers.get("set-cookie")).toContain("Max-Age=0");
  });
});
