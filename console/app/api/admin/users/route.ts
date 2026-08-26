// app/api/admin/users/route.ts — BFF proxy for the admin Users list + create (story 8.15).
//
// The Users & Roles screen (admin-only) reads the fleet's users and can mint a new one. The browser
// talks ONLY to this BFF route (ADR-013 single choke point); it forwards the session cookie upstream
// to the apiserver's /admin/users, which enforces requireAdmin (global_role=admin) and the
// same-origin guard. Status is relayed VERBATIM — a non-admin caller gets the apiserver's 401/403
// untouched, so the console never fabricates access. The nav already hides this surface for
// non-admins (8.16); this route is the real wall's mirror, not a second authz path.

import type { NextRequest } from "next/server";
import { proxyJson, proxyJsonWrite } from "@/lib/bff";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

export async function GET(req: NextRequest): Promise<Response> {
  // Forward pagination (?limit=&offset=) verbatim to the upstream list.
  return proxyJson(req, `/admin/users${req.nextUrl.search}`);
}

export async function POST(req: NextRequest): Promise<Response> {
  return proxyJsonWrite(req, "/admin/users", "POST");
}
