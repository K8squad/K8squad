// app/api/runs/[runId]/build/[resource]/route.ts — BFF build-browser proxy (story 8.7d).
//
// GET-ONLY. Four read resources — tree | diff | file | meta — proxied to the Go apiserver's
// build-browser read-model. The AUTHORITATIVE per-principal authZ gate (NFR-SEC5) lives in the
// apiserver (single source of truth, arch §13); this BFF route forwards the caller's session
// identity and surfaces the apiserver's response VERBATIM. Critically: a 404 stays a 404
// (existence-hiding — a same-Team non-owner MUST NOT be able to tell deny from not-found), never
// re-mapped to 403. No mutating verb is routed here (POST/PUT/PATCH/DELETE are structurally
// absent → 405), matching AC1.

import type { NextRequest } from 'next/server';
import { proxyJson } from '@/lib/bff';

export const dynamic = 'force-dynamic';
export const runtime = 'nodejs';
export const fetchCache = 'force-no-store';

const RESOURCES = new Set(['tree', 'diff', 'file', 'meta']);

export async function GET(
  req: NextRequest,
  { params }: { params: Promise<{ runId: string; resource: string }> },
): Promise<Response> {
  const { runId: rawRunId, resource } = await params;
  // Unknown resource → 404 (indistinguishable from a denied/missing Run — existence-hiding).
  if (!RESOURCES.has(resource)) {
    return new Response(null, { status: 404 });
  }
  const runId = encodeURIComponent(rawRunId);
  // Forward the caller's query (?path=, ?ref=run|base) verbatim; the apiserver's read-model
  // resolves paths structurally through git (never raw FS) and applies the caps (8.7a).
  const search = req.nextUrl.search; // includes leading '?' or ''
  return proxyJson(req, `/api/runs/${runId}/build/${resource}${search}`);
}
