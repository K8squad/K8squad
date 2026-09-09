// app/api/projects/repo-auth/test/route.ts — BFF proxy for the repo-auth
// test-connection probe (POST, ISI-4000 S2 → ISI-3683 repoauthtest.go).
//
// POST /api/projects/repo-auth/test → apiserver POST /api/projects/repo-auth/test.
// The apiserver runs a SERVER-SIDE probe of a STORED credential against a repo URL
// (the token is read from the Secret, used once, and NEVER crosses a response
// field or log line) and answers {ok, detail}. The §13 choke point applies the
// real authz + tenancy scoping upstream; the BFF forwards identity + body
// unchanged and relays the status VERBATIM — an honest {ok:false} (e.g. "Rejected
// HTTP 401") is a valid outcome, never suppressed. The 501 (prober not wired in a
// cluster-less dev run) stays 501.
//
// This is a state-adjacent write (it reads a Secret and hits an external host), so
// it is gated same-origin FIRST — the same login-CSRF posture the credential write
// uses — before any upstream call. PUT/PATCH/DELETE stay structurally absent → 405.

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
