// app/api/modelendpoints/providers/route.ts — BFF proxy for the data-only provider registry
// (ISI-5005) the ProviderModelPicker draws from: {providers:[{id,label,mode,wire,curatedOnly,
// allowKey}]}. Session-gated (static metadata, no credential, no per-tenant fact) — the
// apiserver applies the real authz behind the §13 choke point; the BFF relays VERBATIM. GET
// only; other methods stay structurally absent → 405.

import type { NextRequest } from "next/server";
import { proxyJson } from "@/lib/bff";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

export async function GET(req: NextRequest): Promise<Response> {
  return proxyJson(req, "/api/modelendpoints/providers");
}
