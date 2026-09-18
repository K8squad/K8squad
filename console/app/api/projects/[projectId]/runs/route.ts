// app/api/projects/[projectId]/runs/route.ts — ISI-4571 project-scoped run listing proxy.
//
// Routes the console's project run listing page to the apiserver's
// GET /api/projects/{projectId}/runs endpoint. Reuses the proxyJson pattern to
// forward session identity; the apiserver owns the authoritative authZ gate
// (requireProjectRole(viewer)). Query params (phase/agent/window/limit/offset)
// are forwarded verbatim.

import type { NextRequest } from "next/server";
import { proxyJson } from "@/lib/bff";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

export async function GET(
  req: NextRequest,
  { params }: { params: Promise<{ projectId: string }> },
): Promise<Response> {
  const projectId = encodeURIComponent((await params).projectId);
  const search = new URL(req.url).search;
  return proxyJson(req, `/api/projects/${projectId}/runs${search}`);
}
