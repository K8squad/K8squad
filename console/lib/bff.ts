// lib/bff.ts — the console's Backend-For-Frontend edge.
//
// Architecture §13 / ADR-013: the browser NEVER talks to the Go apiserver (or kube, or
// Postgres) directly. The Next.js server is the ONE authorization choke point — it holds the
// HttpOnly session cookie and forwards it upstream, where the apiserver mints the internal JWT
// and applies the §12.3 deny-by-default RBAC wall. The BFF adds NO second authz path and NO
// client-side authz: it forwards the caller's identity and surfaces the apiserver's decision
// verbatim (a 404 stays a 404 — existence-hiding per NFR-SEC5 / story 8.7d), never re-mapping
// status codes or leaking the apiserver's origin to the browser.
//
// This module is SERVER-ONLY. It must never be imported into a client component.

import "server-only";
import type { NextRequest } from "next/server";

/** Base URL of the Go apiserver. Server-only — never exposed to the browser (no NEXT_PUBLIC_*). */
export function apiserverBaseUrl(): string {
  return process.env.KSQUAD_APISERVER_URL ?? "http://ksquad-apiserver:8080";
}

/** Name of the HttpOnly session cookie the BFF forwards upstream (arch §12.3 / ADR-033). */
export function sessionCookieName(): string {
  return process.env.KSQUAD_SESSION_COOKIE ?? "ksquad_session";
}

/**
 * Build the upstream headers for a proxied request: forward the caller's session cookie (the
 * identity the apiserver resolves), the SSE resume header, and content negotiation. We forward
 * identity ONLY — no BFF-asserted principal, so the apiserver never trusts a BFF-fabricated
 * caller for the owning-principal check (story 8.7d Dev Notes).
 */
function upstreamHeaders(
  req: NextRequest,
  extra?: Record<string, string>,
): Headers {
  const h = new Headers();
  const cookie = req.headers.get("cookie");
  if (cookie) h.set("cookie", cookie);
  // SSE reconnect: carry Last-Event-ID so the apiserver resumes the durable coord-record tail
  // (story 8.2 AC5 — no loss, no gap).
  const lastEventId = req.headers.get("last-event-id");
  if (lastEventId) h.set("last-event-id", lastEventId);
  const accept = req.headers.get("accept");
  if (accept) h.set("accept", accept);
  if (extra) for (const [k, v] of Object.entries(extra)) h.set(k, v);
  return h;
}

/**
 * Proxy an SSE stream from the apiserver to the browser UNBUFFERED (story 8.2 AC2/AC3).
 *
 * The returned Response streams the upstream body through incrementally — each event flushes to
 * the browser as it arrives (no batch-until-close). We set text/event-stream and disable any
 * intermediary buffering (`X-Accel-Buffering: no`). The apiserver's status is preserved, so an
 * RBAC deny (§12.3) or a not-found Run surfaces verbatim rather than being masked by a 200 shell.
 */
export async function proxyEventStream(
  req: NextRequest,
  upstreamPath: string,
): Promise<Response> {
  const url = apiserverBaseUrl() + upstreamPath;
  const upstream = await fetch(url, {
    method: "GET",
    headers: upstreamHeaders(req, { accept: "text/event-stream" }),
    // Do not let fetch/undici buffer or cache; stream the body through.
    cache: "no-store",
    signal: req.signal,
  });

  // Non-2xx (RBAC deny, 404 existence-hiding, upstream error): surface verbatim, no stream.
  if (!upstream.ok || !upstream.body) {
    return new Response(upstream.body, {
      status: upstream.status,
      statusText: upstream.statusText,
      headers: { "cache-control": "no-store" },
    });
  }

  return new Response(upstream.body, {
    status: 200,
    headers: {
      "content-type": "text/event-stream; charset=utf-8",
      "cache-control": "no-cache, no-transform",
      connection: "keep-alive",
      // Defeat proxy buffering (nginx/ingress) so "live" stays incremental (AC3 / arch §16.1).
      "x-accel-buffering": "no",
    },
  });
}

/**
 * Proxy a GET-only JSON request to the apiserver, surfacing status VERBATIM.
 *
 * Used by the build-browser read endpoints (story 8.7d): the authoritative per-principal
 * authZ gate lives in the Go apiserver read-model; the BFF forwards the caller identity and
 * relays the apiserver's response — critically, a 404 is relayed as a 404 (existence-hiding,
 * NEVER re-mapped to 403), so a same-Team non-owner cannot distinguish deny from not-found.
 */
export async function proxyJson(
  req: NextRequest,
  upstreamPath: string,
): Promise<Response> {
  const url = apiserverBaseUrl() + upstreamPath;
  const upstream = await fetch(url, {
    method: "GET",
    headers: upstreamHeaders(req, { accept: "application/json" }),
    cache: "no-store",
    signal: req.signal,
  });

  const body = await upstream.arrayBuffer();
  return new Response(body, {
    status: upstream.status,
    statusText: upstream.statusText,
    headers: {
      "content-type":
        upstream.headers.get("content-type") ?? "application/json",
      "cache-control": "no-store",
    },
  });
}

/**
 * Proxy an AUTH mutation (login/logout) to the apiserver, RELAYING its Set-Cookie (ISI-3522).
 *
 * The auth routes are the session cookie's ISSUER: `POST /auth/login` sets the HttpOnly opaque
 * `ksquad_session`, `POST /auth/logout` expires it. Unlike proxyJson*, this helper copies the
 * apiserver's Set-Cookie header(s) back to the browser so the cookie actually lands (login) or is
 * cleared (logout). It still adds NO second authz path — the apiserver is the sole credential
 * verifier and cookie authority (§13 / ADR-013); the console never mints or reads the token. Status
 * (401 invalid creds, 429 rate-limited, 200) is surfaced verbatim.
 */
export async function proxyAuth(
  req: NextRequest,
  upstreamPath: string,
  method: "POST" | "DELETE",
): Promise<Response> {
  const url = apiserverBaseUrl() + upstreamPath;
  const inboundBody = await req.text();
  const upstream = await fetch(url, {
    method,
    headers: upstreamHeaders(req, {
      accept: "application/json",
      "content-type": req.headers.get("content-type") ?? "application/json",
    }),
    body: inboundBody.length > 0 ? inboundBody : undefined,
    cache: "no-store",
    signal: req.signal,
  });

  const raw = await upstream.arrayBuffer();
  const headers = new Headers({
    "content-type":
      upstream.headers.get("content-type") ?? "application/json",
    "cache-control": "no-store",
  });
  // Relay every Set-Cookie verbatim (the session cookie lands/clears here, HttpOnly attrs intact).
  for (const cookie of upstream.headers.getSetCookie()) {
    headers.append("set-cookie", cookie);
  }
  // 204/205/304 are null-body statuses — logout answers 204 — so a non-null body (even a 0-byte
  // ArrayBuffer) makes the Response constructor throw. Pass null for those.
  const nullBody = upstream.status === 204 || upstream.status === 205 || upstream.status === 304;
  return new Response(nullBody ? null : raw, {
    status: upstream.status,
    statusText: upstream.statusText,
    headers,
  });
}

/**
 * Proxy a JSON *mutation* (POST/PUT/PATCH/DELETE) to the apiserver, surfacing status VERBATIM.
 *
 * Used by write surfaces that must still traverse the ONE authz choke point (arch §13 / ADR-013):
 * e.g. posting a discussion message (story 10.3 AC3/AC4). The BFF forwards the caller's session
 * identity and the request body UNCHANGED — it adds no BFF-asserted principal and stamps no
 * provenance. Provenance (author_*) is stamped SERVER-SIDE from the authenticated principal, so a
 * client body of `{ body, parentId? }` is relayed as-is and the apiserver is the sole author of
 * attribution. As with reads, a deny is existence-hiding: a 404 (or 401/403) is relayed verbatim
 * and never re-mapped, so a foreign-Project write cannot distinguish deny from not-found.
 */
export async function proxyJsonWrite(
  req: NextRequest,
  upstreamPath: string,
  method: "POST" | "PUT" | "PATCH" | "DELETE",
): Promise<Response> {
  const url = apiserverBaseUrl() + upstreamPath;
  // Forward the caller's raw body unchanged; the apiserver validates + server-stamps provenance.
  const inboundBody = await req.text();
  const contentType = req.headers.get("content-type") ?? "application/json";
  const upstream = await fetch(url, {
    method,
    headers: upstreamHeaders(req, {
      accept: "application/json",
      "content-type": contentType,
    }),
    body: inboundBody.length > 0 ? inboundBody : undefined,
    cache: "no-store",
    signal: req.signal,
  });

  const body = await upstream.arrayBuffer();
  return new Response(body, {
    status: upstream.status,
    statusText: upstream.statusText,
    headers: {
      "content-type":
        upstream.headers.get("content-type") ?? "application/json",
      "cache-control": "no-store",
    },
  });
}
