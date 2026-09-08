// app/api/teams/route.ts — BFF proxy for the Teams LIST read model (ISI-3953,
// gap G4 of the ISI-3949 fleet-admin audit). GET-ONLY.
//
// Proxies the Go apiserver's GET /api/teams: admin ⇒ every Team (fleet), tenant
// ⇒ own Team only (existence-hiding). The §13 choke point applies the real authz
// + tenancy scoping THERE; this route forwards the caller's session identity and
// surfaces the apiserver's response VERBATIM — including its documented 501 when
// no read model is wired (cluster-less run), which the screen renders as an
// honest "not configured" state, never a fabricated list. No mutating verb is
// routed here — POST /api/teams is the separate compose collection, reached
// through its own BFF path, so POST/PUT/PATCH/DELETE are structurally absent
// here → 405.

import type { NextRequest } from "next/server";
import { proxyJson } from "@/lib/bff";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

export async function GET(req: NextRequest): Promise<Response> {
  return proxyJson(req, "/api/teams");
}
