// app/api/projects/[projectId]/github/route.ts — BFF proxy for the GitHub-status
// read (GET, ISI-3956 S5c / S5b).
//
// The browser talks ONLY to this Next.js server; the request is proxied to the Go
// apiserver's GitHub-status read model (githubstatus.go), which owns the
// AUTHORITATIVE deny-by-default authZ gate (§12.3) and reads the scm mirror — the
// apiserver is the wall: it makes NO GitHub call and NEVER returns the BYO repo
// credential. The BFF forwards the caller's session cookie and surfaces the
// apiserver's status VERBATIM — a deny is existence-hiding (404 stays 404, never
// re-mapped), and the documented 501 (read model not yet wired, S5b pending)
// stays 501 so the tab renders its honest "not available yet" state. Pass-through
// only: no verb other than GET is routed here.

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
  return proxyJson(req, `/api/projects/${projectId}/github`);
}
