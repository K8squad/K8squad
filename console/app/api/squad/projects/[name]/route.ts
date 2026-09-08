// app/api/squad/projects/[name]/route.ts — BFF project-authoring-detail proxy (ISI-4002, ADR-0016).
//
// GET-ONLY. Proxies the apiserver's per-kind authoring-spec read
// (GET /api/squad/projects/{name}, fleetlist.go) — the compose WRITE wire shape the
// Compose EDIT form hydrates from so opening a project shows its real spec and a PUT
// does not blow it away (the empty-form-on-edit bug, ISI-3985). Scoping is AUTHORITATIVE
// server-side: a tenant resolves {name} in their own squad; a fleet admin appends
// ?team={teamUid} to select the squad namespace (the same act-as-team selector ISI-3955
// reuses for writes). A miss/foreign name is existence-hiding (404, never 403). This route
// forwards the caller's session identity + the ?team= selector and surfaces the response
// VERBATIM. No mutating verb is routed here → POST/PUT/PATCH/DELETE = 405.

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
  return proxyJson(req, `/api/squad/projects/${name}${qs}`);
}
