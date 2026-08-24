// app/api/runs/[runId]/artifacts/[artifactId]/route.ts — BFF artifact content proxy (8.3).
//
// GET-ONLY. Proxies one artifact's capped canonical bytes (digest-verified at the uri the
// coordination record registers). The artifact is resolved WITHIN the gated Run's rows by
// the apiserver, so a guessed id from another Run is indistinguishable from a missing one
// (existence-hiding). The apiserver's status is surfaced VERBATIM — 404 stays a 404.

import type { NextRequest } from "next/server";
import { proxyJson } from "@/lib/bff";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

export async function GET(
  req: NextRequest,
  { params }: { params: Promise<{ runId: string; artifactId: string }> },
): Promise<Response> {
  const { runId, artifactId } = await params;
  return proxyJson(
    req,
    `/api/runs/${encodeURIComponent(runId)}/artifacts/${encodeURIComponent(artifactId)}`,
  );
}
