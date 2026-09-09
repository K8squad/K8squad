// app/api/projects/repo-auth/test/route.ts — BFF repo-auth Test-connection proxy (ISI-4000 / S2).
//
// POST-ONLY. Proxies the apiserver's server-side repo credential probe
// (POST /api/projects/repo-auth/test, repoauthtest.go): the caller sends {url, credentialSecretRef}
// — NEVER a token — and the apiserver resolves the STORED Secret in the caller's team namespace,
// probes GitHub (GET /user), and answers {ok, detail}. The §13 choke point owns authz + tenancy;
// the BFF forwards identity + body unchanged and surfaces the apiserver's response VERBATIM
// (200 {ok,detail}, 422 field errors, 404 credential-not-found / existence-hiding, 502 grant/
// transport, 401). detail is safe to render verbatim (E5 discipline): it names the login/count or
// the failing step, never the credential material.
//
// The probe reads a stored Secret, so it is gated same-origin FIRST (crossSiteReject, the login-
// CSRF posture the credentials write uses) before any upstream call.

import type { NextRequest } from "next/server";
import { crossSiteReject, proxyJsonWrite } from "@/lib/bff";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

export async function POST(req: NextRequest): Promise<Response> {
  const rejected = crossSiteReject(req);
  if (rejected) return rejected;
  return proxyJsonWrite(req, "/api/projects/repo-auth/test", "POST");
}
