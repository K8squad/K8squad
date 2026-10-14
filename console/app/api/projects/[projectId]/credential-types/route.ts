// app/api/projects/[projectId]/credential-types/route.ts — BFF proxy for the
// WS-9 S1 credential-types READ (GET, ISI-5811) consumed by the S4 Credentials
// Vault UI (ISI-5847).
//
// The browser talks ONLY to this Next.js server; the request is proxied to the Go
// apiserver's per-Project credential-types read model (credentialtypes.go), which
// owns the AUTHORITATIVE authZ + tenancy gate (the SAME resolve spine the settings
// and dashboard reads ride — team-fenced 404 existence-hiding for a tenant,
// fleet-wide 409-on-collision for an admin) and returns NO Secret value and NO
// Secret name by design — only the consumable types + their go-live gate-state.
// The BFF forwards the caller's session cookie and surfaces the apiserver's status
// VERBATIM (including the documented 501 in a cluster-less dev run, so the card
// renders its honest "not available yet" frame). GET-only: no other verb is routed.

import type { NextRequest } from "next/server";
import { proxyJson } from "@/lib/bff";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

export async function GET(
  req: NextRequest,
  { params }: { params: Promise<{ projectId: string }> },
): Promise<Response> {
  const projectId = encodeURIComponent((await params).projectId);
  return proxyJson(req, `/api/projects/${projectId}/credential-types`);
}
