// app/api/compose/squad/route.ts — BFF proxy for the squad materialize endpoint (ISI-3677, AD-3).
//
// POST /api/compose/squad → apiserver POST /api/compose/squad: one authorized call turns a
// template (Minimal Trio ★ / BMAD / Solo) into a Team (if absent) + N Agents, each referencing a
// seeded Role preset and the ONE shared credentialSecretRef (AD-5). The browser talks ONLY to this
// Next.js server; the write still traverses the ONE authz choke point (§13 / ADR-013) — the BFF
// forwards the caller's session identity and the request body UNCHANGED (the apiserver validates,
// RBAC-gates, team-scopes, and server-stamps provenance). Status surfaces VERBATIM: 201 full
// materialize, 207 partial (per-object failures in `errors`, NFR-5), 422 template/field errors,
// 401/403, 404 (cross-tenant / no team namespace), and 501 (cluster-less default) all reach the
// screen untouched so the template gallery can render the honest result (E2-S3).

import type { NextRequest } from "next/server";
import { proxyJsonWrite } from "@/lib/bff";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

export async function POST(req: NextRequest): Promise<Response> {
  return proxyJsonWrite(req, "/api/compose/squad", "POST");
}
