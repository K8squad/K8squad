// app/api/credentials/route.ts — BFF credential read + BYO-write proxy (story 8.6 / ISI-3983).
//
// GET proxies the Go apiserver's Team-scoped credential read model; POST proxies the BYO
// service-account write (POST /api/credentials, ISI-3679/ISI-3937). PUT rotates and DELETE
// removes a vault tool-credential (ISI-5860 / design O-2, backed by ISI-5816): rotation is a
// delete-then-recreate under the SAME deterministic Secret name (its own `ksquad.credential.rotate`
// span), delete is a blind delete-by-{type} (its own `ksquad.credential.delete` span). The §13
// choke point applies the real authz + tenancy scoping in the apiserver — the BFF forwards the
// caller's session identity and the request body UNCHANGED, and surfaces the apiserver's response
// VERBATIM (its documented 501 for the read model or a human-seat class, its 400 "select a team"
// for a fleet admin who omitted a team, and — critically — a 404 delete stays a 404, existence-
// hiding, NEVER re-mapped). The write value is relayed once, stored server-side, and NEVER
// serialised back on ANY verb (NFR-2 — structural: the apiserver SA has no `get`). PATCH stays
// structurally absent → 405.
//
// POST/PUT/DELETE are state-changing writes that land or remove a Kubernetes Secret, so EACH is
// gated same-origin FIRST (crossSiteReject — the same login-CSRF posture proxyAuth uses) before
// any upstream call: a cross-site form must not be able to plant, replace, or destroy a credential
// under the victim's session.

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

export async function PUT(req: NextRequest): Promise<Response> {
  const rejected = crossSiteReject(req);
  if (rejected) return rejected;
  return proxyJsonWrite(req, "/api/credentials", "PUT");
}

export async function DELETE(req: NextRequest): Promise<Response> {
  const rejected = crossSiteReject(req);
  if (rejected) return rejected;
  return proxyJsonWrite(req, "/api/credentials", "DELETE");
}
