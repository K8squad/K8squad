// app/api/inbox/seen/route.ts — BFF proxy for the Inbox mark-read write (ISI-5535, ADR-0026 §6).
//
// POST-ONLY. Proxies the Go apiserver's POST /api/squad/inbox/seen, which upserts per-user
// read-markers (seen_at = now()) for the supplied item keys so the unread dots + nav badge clear.
// The AuthorContext (user principal) is resolved server-side from the ksquad_session cookie at the
// §13 BFF choke point. As a state-changing write it is gated same-origin FIRST (crossSiteReject,
// same posture as the model-endpoint write) before any upstream call.

import type { NextRequest } from "next/server";
import { crossSiteReject, proxyJsonWrite } from "@/lib/bff";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

export async function POST(req: NextRequest): Promise<Response> {
  const rejected = crossSiteReject(req);
  if (rejected) return rejected;
  return proxyJsonWrite(req, "/api/squad/inbox/seen", "POST");
}
