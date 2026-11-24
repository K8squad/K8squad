// app/api/instscore/services/route.ts — BFF proxy for the instrumentation-score
// services list (ISI-7750, phase 4 of the ISI-7741 plan).
//
// GET-ONLY. Proxies the apiserver's read model behind the §13 choke point (the
// session identity resolves there; the score store is fleet-wide by design —
// the OTel gateway tap of ISI-7749 wrote every row). The apiserver's response —
// including the documented 501 when no store is wired — is relayed verbatim. No
// mutating verb is routed here (POST/PUT/PATCH/DELETE are structurally absent → 405).

import type { NextRequest } from "next/server";
import { proxyJson } from "@/lib/bff";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

export async function GET(req: NextRequest): Promise<Response> {
  return proxyJson(req, "/api/v1/instscore/services");
}
