// app/api/projects/[projectId]/github/sync/route.ts — BFF proxy for the
// "Sync now" trigger (POST, ISI-4011).
//
// Forwards to the Go apiserver's POST /api/projects/{id}/github/sync which
// bumps the scm-sync-trigger annotation. The apiserver owns the authz gate
// (contributor+) and the 30-s per-project debounce; the BFF passes status
// verbatim — 202 Accepted on success, 429 with Retry-After on debounce,
// 403/404/501 surfaced as-is. POST only.

import type { NextRequest } from "next/server";
import { proxyJsonWrite } from "@/lib/bff";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";

export async function POST(
  req: NextRequest,
  { params }: { params: Promise<{ projectId: string }> },
): Promise<Response> {
  const projectId = encodeURIComponent((await params).projectId);
  return proxyJsonWrite(req, `/api/projects/${projectId}/github/sync`, "POST");
}
