// app/api/compose/[kind]/[name]/route.ts — BFF proxy for EDIT on the Compose write surface (story 8.5).
//
// PUT /api/compose/{kind}/{name} → apiserver PUT /api/{kind}/{name} (ISI-3198). An edit is an
// idempotent upsert keyed on (kind, team, name) that stamps a NEW revision — never an in-place
// mutation of a running snapshot (§6.4). The BFF forwards identity + body unchanged; status is
// relayed VERBATIM (200 updated, 409 concurrent-modification, 422 field/admission errors,
// 401/403/404, 501). Both `kind` (fixed allow-list) and `name` (percent-encoded) are constrained
// so neither can inject an arbitrary upstream path.

import type { NextRequest } from "next/server";
import { proxyJsonWrite } from "@/lib/bff";
import { isComposeKind } from "@/lib/compose";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

export async function PUT(
  req: NextRequest,
  { params }: { params: Promise<{ kind: string; name: string }> },
): Promise<Response> {
  const { kind, name } = await params;
  if (!isComposeKind(kind)) {
    return Response.json({ error: "unknown compose kind" }, { status: 404 });
  }
  return proxyJsonWrite(req, `/api/${kind}/${encodeURIComponent(name)}`, "PUT");
}
