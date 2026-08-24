// app/api/session/route.ts — BFF proxy for the caller's session/role summary
// (story 8.14b UI RBAC gate). GET-ONLY.
//
// The console needs the caller's role (viewer vs contributor/maintainer) to
// decide whether drag-and-drop is ENABLED in the UI. The authoritative RBAC
// decision always lives in the Go apiserver (§6.7.2/§12.3) — this gate is a UX
// mirror, never a security boundary. The BFF relays the apiserver's /api/me
// verdict verbatim; the client FAILS CLOSED to viewer on any non-200 or error
// (deny-by-default), so an absent upstream never widens permissions.

import type { NextRequest } from "next/server";
import { proxyJson } from "@/lib/bff";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

export async function GET(req: NextRequest): Promise<Response> {
  return proxyJson(req, "/api/me");
}
