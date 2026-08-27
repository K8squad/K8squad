// app/api/telemetry/tool-usage/route.ts — BFF tool-usage proxy (Epic D / ISI-3288, D3).
//
// GET-ONLY. Proxies the Go apiserver's per-agent tool-usage read model (the
// aggregate of the operator's ksquad_* tool metrics). The BFF forwards the
// caller's session identity and surfaces the apiserver's response VERBATIM —
// including its documented 501 (read model not wired) and 503 (operator
// metrics unreachable), which the panel renders as an honest degraded state.
// The `?agent=` query scopes the per-agent array upstream. No mutating verb
// is routed here (POST/PUT/PATCH/DELETE are structurally absent → 405).

import type { NextRequest } from "next/server";
import { proxyJson } from "@/lib/bff";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

export async function GET(req: NextRequest): Promise<Response> {
  const agent = req.nextUrl.searchParams.get("agent");
  const qs = agent ? `?agent=${encodeURIComponent(agent)}` : "";
  return proxyJson(req, `/api/telemetry/tool-usage${qs}`);
}
