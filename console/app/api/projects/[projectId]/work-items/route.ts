// app/api/projects/[projectId]/work-items/route.ts — BFF proxy for the Project
// Tickets read (GET, stories 8.14b–d / 8.17) + human CREATE (POST, S3/ISI-3959).
//
// The browser talks ONLY to this Next.js server; the work-item list (and the
// `?parentId=` lazy-load for the sub-ticket tree) is proxied to the Go apiserver's
// work-items read-model, which owns the AUTHORITATIVE deny-by-default authZ gate
// (§12.3 / ADR-013) and the tenancy-scoped query predicates (§12.1). The BFF
// forwards the caller's session identity and the query string VERBATIM and
// surfaces the apiserver's response VERBATIM — a deny is existence-hiding (404
// stays 404, never re-mapped), and a documented 501 (read model not yet hosted,
// ISI-2909) stays 501 so the UI can render its honest "not available yet" state.
//
// POST is the S3 human create (ISI-3959): it forwards the body + session cookie
// unchanged; the apiserver is the sole authority for the contributor+ project-role
// wall, the human-only gate, tenancy scoping, and the §6.5 audit row. Status is
// relayed VERBATIM (201 created / 400 / 403 viewer / 404 existence-hiding / 501
// store-less) so the console never fabricates a create. No other verb is routed
// here (field-edit is PATCH /api/work-items/{id}).

import type { NextRequest } from "next/server";
import { proxyJson, proxyJsonWrite } from "@/lib/bff";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

export async function GET(
  req: NextRequest,
  { params }: { params: Promise<{ projectId: string }> },
): Promise<Response> {
  const projectId = encodeURIComponent((await params).projectId);
  // Forward the server-side filter/sort/parentId predicates unchanged (8.14d AC1).
  const search = req.nextUrl.search;
  return proxyJson(req, `/api/projects/${projectId}/work-items${search}`);
}

export async function POST(
  req: NextRequest,
  { params }: { params: Promise<{ projectId: string }> },
): Promise<Response> {
  const projectId = encodeURIComponent((await params).projectId);
  return proxyJsonWrite(req, `/api/projects/${projectId}/work-items`, "POST");
}
