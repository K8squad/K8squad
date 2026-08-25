// app/api/compose/[kind]/route.ts — BFF proxy for CREATE on the Compose write surface (story 8.5).
//
// POST /api/compose/{kind} → apiserver POST /api/{kind} (kind ∈ teams|projects|agents|roles|skills),
// the CRD-apply write surface (ISI-3198). The browser talks ONLY to this Next.js server; the write
// still traverses the ONE authz choke point (§13 / ADR-013) — the BFF forwards the caller's session
// identity and the request body UNCHANGED (the apiserver validates, RBAC-gates, team-scopes, and
// server-stamps provenance). Status surfaces VERBATIM: 201 create, 422 field errors, 401/403 (viewer),
// 404 (existence-hiding / cross-tenant / no team namespace), 409 (already exists), and 501 (the
// documented cluster-less default, or a not-yet-registered route before ISI-3198 lands) all reach the
// screen untouched. `kind` is validated against the fixed allow-list so it can never inject an
// arbitrary upstream path.

import type { NextRequest } from "next/server";
import { proxyJsonWrite } from "@/lib/bff";
import { isComposeKind } from "@/lib/compose";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

export async function POST(
  req: NextRequest,
  { params }: { params: Promise<{ kind: string }> },
): Promise<Response> {
  const { kind } = await params;
  if (!isComposeKind(kind)) {
    return Response.json({ error: "unknown compose kind" }, { status: 404 });
  }
  return proxyJsonWrite(req, `/api/${kind}`, "POST");
}
