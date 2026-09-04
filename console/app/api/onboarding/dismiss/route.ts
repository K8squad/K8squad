// app/api/onboarding/dismiss/route.ts — BFF onboarding-dismiss write proxy (E1-S4, ISI-3761, FR-1.3).
//
// POST-only. Proxies the Go apiserver's server-side dismissal write-path
// (POST /api/onboarding/dismiss, internal/apiserver/onboarding.go) so a returning tenant's
// Launchpad dismissal persists across devices — the "Finish setup (n/4)" chip (E1-S3) reads the
// SERVER flag this endpoint sets. The GET /api/onboarding/progress projection only READS the flag;
// this is its sole writer (the BFF is otherwise GET-only by design).
//
// The AUTHORITATIVE Team scoping lives in the apiserver: the caller's ksquad_session cookie
// resolves to an AuthorContext whose TeamID scopes the write server-side — no {teamId} param, so a
// cross-tenant write is structurally impossible. This route forwards the session identity + the raw
// {dismissed:bool} body and surfaces the apiserver's response VERBATIM (200 {dismissed}, 401
// unauthenticated, 404 first-run tenant with no Team CR, 501 write client not wired).
//
// CSRF posture: proxyJsonWrite forwards the Origin header on the state-changing POST; the apiserver
// sameOriginGuard rejects a cross-origin write. localStorage stays the offline floor (E1-S2); this
// is the durable cross-device write.

import type { NextRequest } from "next/server";
import { proxyJsonWrite } from "@/lib/bff";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

export async function POST(req: NextRequest): Promise<Response> {
  return proxyJsonWrite(req, "/api/onboarding/dismiss", "POST");
}
