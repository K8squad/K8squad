// app/api/credentials/route.ts — BFF credential read + BYO-write proxy (story 8.6 / ISI-3983).
//
// GET proxies the Go apiserver's Team-scoped credential read model; POST proxies the BYO
// service-account write (POST /api/credentials, ISI-3679/ISI-3937). The §13 choke point applies
// the real authz + tenancy scoping in the apiserver — the BFF forwards the caller's session
// identity and the request body UNCHANGED, and surfaces the apiserver's response VERBATIM
// (including its documented 501 for the read model or a human-seat class, and its 400
// "select a team" for a fleet admin who omitted a team). The write value is relayed once,
// stored server-side, and NEVER serialised back (NFR-2). PUT/PATCH/DELETE stay structurally
// absent → 405.
//
// The POST is a state-changing write that lands a Kubernetes Secret, so it is gated
// same-origin FIRST (crossSiteReject — the same login-CSRF posture proxyAuth uses) before any
// upstream call: a cross-site form must not be able to plant a credential under the victim's
// session.

import type { NextRequest } from "next/server";
import { crossSiteReject, proxyJson, proxyJsonWrite } from "@/lib/bff";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

export async function GET(req: NextRequest): Promise<Response> {
  return proxyJson(req, "/api/credentials");
}

export async function POST(req: NextRequest): Promise<Response> {
  const rejected = crossSiteReject(req);
  if (rejected) return rejected;
  return proxyJsonWrite(req, "/api/credentials", "POST");
}
