// app/api/inbox/unseen/route.ts — BFF proxy for the Inbox mark-UNREAD write (ISI-5537 E3, ADR-0026 §6).
//
// POST-ONLY. Proxies the Go apiserver's POST /api/squad/inbox/unseen, which DROPS the per-user
// read-markers for the supplied item keys so they re-surface as unread (the "mark unread" half of the
// read/unread toggle; the "mark read" half is ./seen/route.ts). The AuthorContext (user principal) is
// resolved server-side from the ksquad_session cookie at the §13 BFF choke point. As a state-changing
// write it is gated same-origin FIRST (crossSiteReject), mirroring the seen route.

import type { NextRequest } from "next/server";
import { crossSiteReject, proxyJsonWrite } from "@/lib/bff";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

export async function POST(req: NextRequest): Promise<Response> {
  const rejected = crossSiteReject(req);
  if (rejected) return rejected;
  return proxyJsonWrite(req, "/api/squad/inbox/unseen", "POST");
}
