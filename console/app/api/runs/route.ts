// app/api/runs/route.ts — ISI-4571 global run listing proxy.
//
// Routes the console's run listing page to the apiserver's GET /api/runs endpoint.
// Reuses the proxyJson pattern to forward session identity and preserve RBAC scoping.

import type { NextRequest } from "next/server";
import { proxyJson } from "@/lib/bff";

// Never statically cache; run listings are dynamic.
export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

export async function GET(req: NextRequest): Promise<Response> {
  const search = new URL(req.url).search;
  return proxyJson(req, `/api/runs${search}`);
}
