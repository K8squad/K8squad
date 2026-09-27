// app/api/modelendpoints/route.ts — BFF model-endpoint list + write proxy (ISI-5005,
// child of ISI-4989; the deferred ISI-4890 AC6 fast-follow).
//
// GET proxies the Go apiserver's admin-tier list of BYO model endpoints ({name, provider,
// url, hasToken} — never any secret data). POST proxies the endpoint-Secret upsert. The §13
// choke point applies the real authz (admin-tier) + operator-namespace scoping in the
// apiserver; the BFF forwards the caller's session identity and the request body UNCHANGED
// and surfaces the apiserver's response VERBATIM (including its documented 501 on a
// cluster-less dev run, its 403 for a non-admin, and its 422 validation body). The API key is
// relayed once, stored server-side, and NEVER serialised back. PUT/PATCH/DELETE stay
// structurally absent → 405.
//
// The POST is a state-changing write that lands a Kubernetes Secret, so it is gated
// same-origin FIRST (crossSiteReject — the same login-CSRF posture proxyAuth uses) before any
// upstream call: a cross-site form must not be able to plant a model endpoint under the
// victim's session.

import type { NextRequest } from "next/server";
import { crossSiteReject, proxyJson, proxyJsonWrite } from "@/lib/bff";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

export async function GET(req: NextRequest): Promise<Response> {
  return proxyJson(req, "/api/modelendpoints");
}

export async function POST(req: NextRequest): Promise<Response> {
  const rejected = crossSiteReject(req);
  if (rejected) return rejected;
  return proxyJsonWrite(req, "/api/modelendpoints", "POST");
}
