// app/api/projects/[projectId]/settings/route.ts — BFF proxy for the S1 project
// settings READ (GET, ISI-4000 S2 → ISI-3999 S1).
//
// The browser talks ONLY to this Next.js server; the request is proxied to the Go
// apiserver's per-Project settings read model (projectsettings.go), which owns the
// AUTHORITATIVE authZ gate (the SAME requireProjectRole(viewer) as the dashboard —
// no settings-specific path) and NEVER returns token material (NFR-SEC8: the
// projection carries the credential ref NAME + a connected bool only). The BFF
// forwards the caller's session cookie and surfaces the apiserver's status
// VERBATIM — a deny is existence-hiding (404 stays 404) and the documented 501
// (read model not wired in a cluster-less dev run) stays 501 so the tab renders
// its honest "settings not available yet" state. GET-only: no other verb is routed.

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
  return proxyJson(req, `/api/projects/${projectId}/settings`);
}
