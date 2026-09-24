// app/api/projects/[projectId]/repo/review-automation/eligible-agents/route.ts —
// BFF proxy for the D5 eligible-agents read surface (ISI-4779 / ISI-4750 E2).
//
// The browser talks ONLY to this Next.js server; the request is proxied to the Go
// apiserver's review-automation service (reviewautomation.go EligibleAgents),
// which owns the AUTHORITATIVE authZ gate (§12.3 deny-by-default: member+ read)
// and applies the SHARED pkg/reviewauto D5 rule so the roster stays in lockstep
// with the write-time 422. It returns { agents: [{ id, name }] } pre-filtered to
// the project team's code_review-capable agents.
//
// The BFF forwards the caller's session cookie and surfaces the apiserver's status
// VERBATIM (a deny is existence-hiding: 404 stays 404, never re-mapped to 403; the
// documented 501 in a cluster-less dev run stays 501). It adds NO second authz
// path and asserts NO principal (§13 / ADR-013). GET-only: this is a read surface.

import type { NextRequest } from "next/server";
import { proxyJson } from "@/lib/bff";
import { encodeProjectId } from "@/lib/projectId";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

// upstreamPath collapses the Project id to EXACTLY one encoding layer — Next hands
// us the still-encoded "ns%2Fname" segment, and a naïve encodeURIComponent would
// re-encode it to "ns%252Fname", which the apiserver decodes to a literal
// "ns%2Fname" (no slash) and 404s (ISI-3982 / ISI-4662 double-encode trap).
function upstreamPath(projectId: string): string {
  return `/api/projects/${encodeProjectId(projectId)}/repo/review-automation/eligible-agents`;
}

export async function GET(
  req: NextRequest,
  { params }: { params: Promise<{ projectId: string }> },
): Promise<Response> {
  return proxyJson(req, upstreamPath((await params).projectId));
}
