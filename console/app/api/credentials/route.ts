// app/api/credentials/route.ts — BFF credential/auth-state proxy (story 8.6).
//
// GET-ONLY. Proxies the Go apiserver's Team-scoped credential read model (the §13 choke point
// applies the real authz + tenancy scoping there). The BFF forwards the caller's session identity
// and surfaces the apiserver's response VERBATIM — including its documented 501 when the read
// model is not wired (cluster-less run), which the screen renders as an honest "not configured"
// state, never a fabricated table. No mutating verb is routed here (POST/PUT/PATCH/DELETE are
// structurally absent → 405).

import type { NextRequest } from "next/server";
import { proxyJson } from "@/lib/bff";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

export async function GET(req: NextRequest): Promise<Response> {
  return proxyJson(req, "/api/credentials");
}
