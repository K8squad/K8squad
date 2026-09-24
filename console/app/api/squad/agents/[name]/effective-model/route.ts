// app/api/squad/agents/[name]/effective-model/route.ts — BFF effective-model
// read-out proxy (ISI-4892 / S3, epic ISI-4822 Flow C).
//
// GET-ONLY. Proxies the apiserver's read-only effective-model projection
// (GET /api/squad/agents/{name}/effective-model, fleetlist.go), which runs the
// shipped Model-Per-Role resolver (Agent → Role → org-default) and returns
// { model, tier, roleName, fallbackModel?, unresolved? }. The console's
// EffectiveModelReadout renders that verdict + a ProvenanceChip without forking
// the precedence rule into TypeScript. Scoping is AUTHORITATIVE server-side (same
// act-as-team seam as the agent-detail read): a tenant resolves {name} in their
// own squad; a fleet admin appends ?team={teamUid}. A miss/foreign name is
// existence-hiding (404, never 403). No mutating verb is routed here →
// POST/PUT/PATCH/DELETE = 405.

import type { NextRequest } from "next/server";
import { proxyJson } from "@/lib/bff";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

export async function GET(
  req: NextRequest,
  { params }: { params: Promise<{ name: string }> },
): Promise<Response> {
  const name = encodeURIComponent((await params).name);
  const team = req.nextUrl.searchParams.get("team");
  const qs = team ? `?team=${encodeURIComponent(team)}` : "";
  return proxyJson(req, `/api/squad/agents/${name}/effective-model${qs}`);
}
