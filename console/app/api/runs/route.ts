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

export async function GET(
  req: NextRequest,
  { params }: { params: Promise<{ projectId?: string }> },
): Promise<Response> {
  const searchParams = new URL(req.url).searchParams;
  const projectId = (await params).projectId;
  
  let upstreamPath = "";
  if (projectId) {
    upstreamPath = `/api/projects/${encodeURIComponent(projectId)}/runs`;
  } else {
    upstreamPath = "/api/runs";
  }

  // Forward query parameters (filters, pagination)
  const upstreamUrl = new URL(upstreamPath, process.env.KSQUAD_APISERVER_URL ?? "http://ksquad-apiserver:8080");
  searchParams.forEach((value, key) => {
    upstreamUrl.searchParams.set(key, value);
  });

  // Reconstruct path and query string for proxy
  const finalPath = upstreamUrl.pathname + upstreamUrl.search;
  
  return proxyJson(req, finalPath);
}