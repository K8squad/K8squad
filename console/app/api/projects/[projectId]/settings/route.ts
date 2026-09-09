// app/api/projects/[projectId]/settings/route.ts — BFF project-settings READ proxy (ISI-4000 / S2).
//
// GET-ONLY. Proxies the apiserver's S1 read projection (GET /api/projects/{id}/settings,
// projectsettings.go) — the SCM config the Settings tab renders: repo URL/ref, provider, the
// credential status (connected + tri-state last-test), and canEdit. The §13 choke point applies
// the real authz + tenancy scoping in the apiserver; the BFF forwards the caller's session
// identity and surfaces the response VERBATIM, including the documented 501 (read model not wired
// in a cluster-less dev run) and existence-hiding 404s. The projection NEVER carries token
// material (S1 AC2 is structural), so nothing sensitive crosses here. No mutating verb is routed
// → POST/PUT/PATCH/DELETE = 405.

import type { NextRequest } from "next/server";
import { proxyJson } from "@/lib/bff";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

export async function GET(
  req: NextRequest,
  { params }: { params: Promise<{ projectId: string }> },
): Promise<Response> {
  const { projectId } = await params;
  return proxyJson(req, `/api/projects/${encodeURIComponent(projectId)}/settings`);
}
