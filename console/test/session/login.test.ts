import { describe, it, expect, vi, afterEach } from "vitest";
import { NextRequest } from "next/server";
import { GET as sessionGET, POST as sessionPOST, DELETE as sessionDELETE } from "@/app/api/session/route";

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
      makeReq("/api/session", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ username: "x", password: "y" }),
      }),
    );
    expect(res.status).toBe(401);
    expect(res.headers.get("set-cookie")).toBeNull();
  });
});

// ISI-3530: the GET role-summary leg proxies the apiserver's /auth/me — NOT /api/me
// (there is no /api/me route; the stale path 404'd). Guards against reintroducing it.
describe("GET /api/session — the role-summary proxy", () => {
  it("proxies apiserver GET /auth/me (never the stale /api/me)", async () => {
    const fetchMock = vi.fn(async () =>
      new Response(JSON.stringify({ globalRole: "admin" }), {
        status: 200,
        headers: { "content-type": "application/json" },
      }),
    );
    globalThis.fetch = fetchMock as unknown as typeof fetch;

    const res = await sessionGET(makeReq("/api/session", { method: "GET" }));

    const sentUrl = (fetchMock.mock.calls[0] as unknown[])[0] as string;
    expect(sentUrl).toContain("/auth/me");
    expect(sentUrl).not.toContain("/api/me");
    expect(res.status).toBe(200);
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

// Copilot review of PR #215: POST /api/session sets the session cookie, so it is a login-CSRF sink.
// The BFF must reject cross-site submissions and non-JSON bodies BEFORE proxying upstream.
describe("POST /api/session — login-CSRF guard (Copilot PR #215)", () => {
  it("rejects a cross-site fetch-metadata request without hitting the apiserver (403)", async () => {
    const fetchMock = vi.fn(async () => new Response(null, { status: 200 }));
    globalThis.fetch = fetchMock as unknown as typeof fetch;

    const res = await sessionPOST(
      makeReq("/api/session", {
        method: "POST",
        headers: { "content-type": "application/json", "sec-fetch-site": "cross-site" },
        body: JSON.stringify({ username: "op", password: "pw" }),
      }),
    );

    expect(res.status).toBe(403);
    expect(fetchMock).not.toHaveBeenCalled(); // fail closed — no upstream login attempt
    expect(res.headers.get("set-cookie")).toBeNull();
  });

  it("rejects a cross-origin Origin even when fetch metadata is absent (403)", async () => {
    const fetchMock = vi.fn(async () => new Response(null, { status: 200 }));
    globalThis.fetch = fetchMock as unknown as typeof fetch;

    const res = await sessionPOST(
      makeReq("/api/session", {
        method: "POST",
        headers: { "content-type": "application/json", origin: "https://evil.example" },
        body: JSON.stringify({ username: "op", password: "pw" }),
      }),
    );

    expect(res.status).toBe(403);
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("rejects a text/plain body — the classic simple-request CSRF vector (403)", async () => {
    const fetchMock = vi.fn(async () => new Response(null, { status: 200 }));
    globalThis.fetch = fetchMock as unknown as typeof fetch;

    const res = await sessionPOST(
      makeReq("/api/session", {
        method: "POST",
        headers: { "content-type": "text/plain;charset=UTF-8" },
        body: JSON.stringify({ username: "op", password: "pw" }),
      }),
    );

    expect(res.status).toBe(403);
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("allows a same-origin JSON login through to the apiserver", async () => {
    const fetchMock = vi.fn(async () =>
      new Response(JSON.stringify({ ok: true }), {
        status: 200,
        headers: {
          "content-type": "application/json",
          "set-cookie": "ksquad_session=t; Path=/; HttpOnly",
        },
      }),
    );
    globalThis.fetch = fetchMock as unknown as typeof fetch;

    const res = await sessionPOST(
      makeReq("/api/session", {
        method: "POST",
        headers: { "content-type": "application/json", "sec-fetch-site": "same-origin" },
        body: JSON.stringify({ username: "op", password: "pw" }),
      }),
    );

    expect(res.status).toBe(200);
    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(res.headers.get("set-cookie")).toContain("ksquad_session=t");
  });
});

// Copilot review of PR #215: the login proxy dropped X-Forwarded-For, so the apiserver's per-IP
// login limiter saw every user as the BFF pod. The BFF must relay the client-address chain upstream.
describe("POST /api/session — forwards the client-address chain (Copilot PR #215)", () => {
  it("relays X-Forwarded-For / X-Real-IP to the apiserver so the per-IP limiter sees the real caller", async () => {
    const fetchMock = vi.fn(async () => new Response(null, { status: 200 }));
    globalThis.fetch = fetchMock as unknown as typeof fetch;

    await sessionPOST(
      makeReq("/api/session", {
        method: "POST",
        headers: {
          "content-type": "application/json",
          "sec-fetch-site": "same-origin",
          "x-forwarded-for": "203.0.113.7",
          "x-real-ip": "203.0.113.7",
        },
        body: JSON.stringify({ username: "op", password: "pw" }),
      }),
    );

    const init = (fetchMock.mock.calls[0] as unknown[])[1] as { headers: Headers };
    expect(init.headers.get("x-forwarded-for")).toBe("203.0.113.7");
    expect(init.headers.get("x-real-ip")).toBe("203.0.113.7");
  });
});
