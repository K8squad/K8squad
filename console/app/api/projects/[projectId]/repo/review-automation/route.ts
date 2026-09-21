// app/api/projects/[projectId]/repo/review-automation/route.ts — BFF proxy for the
// E1 PR-review-automation config sub-resource (GET read, PUT/PATCH write;
// ISI-4763 / ISI-4750).
//
// The browser talks ONLY to this Next.js server; the request is proxied to the Go
// apiserver's review-automation service (reviewautomation.go), which owns the
// AUTHORITATIVE authZ gate (§12.3 deny-by-default: read = member+, write =
// contributor+) and the D5 reviewer-eligibility validation. The apiserver is the
// wall: it server-stamps the D1 provenance (enabledBy) and NEVER trusts a
// body-supplied value, and it authors no work item / performs no dispatch (writing
// this config is inert until E3/E4 land).
//
// The BFF forwards the caller's session cookie and surfaces the apiserver's status
// VERBATIM — a deny is existence-hiding (404 stays 404, never re-mapped to 403), a
// 422 (bad enum / ineligible reviewer) stays 422, and the documented 501 (service
// not wired in a cluster-less dev run) stays 501 so the E2 UI renders its honest
// "not available yet" state. It adds NO second authz path and asserts NO principal
// (§13 / ADR-013). This is the DEDICATED sub-resource path (E0 §2) — deliberately
// NOT bolted onto the /github status route.

import type { NextRequest } from "next/server";
import { proxyJson, proxyJsonWrite } from "@/lib/bff";
import { encodeProjectId } from "@/lib/projectId";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

// upstreamPath collapses the Project id to EXACTLY one encoding layer — Next hands
// us the still-encoded "ns%2Fname" segment, and a naïve encodeURIComponent would
// re-encode it to "ns%252Fname", which the apiserver decodes to a literal
// "ns%2Fname" (no slash) and 404s (ISI-3982 / ISI-4662 double-encode trap).
function upstreamPath(projectId: string): string {
  return `/api/projects/${encodeProjectId(projectId)}/repo/review-automation`;
}

export async function GET(
  req: NextRequest,
  { params }: { params: Promise<{ projectId: string }> },
): Promise<Response> {
  return proxyJson(req, upstreamPath((await params).projectId));
}

export async function PUT(
  req: NextRequest,
  { params }: { params: Promise<{ projectId: string }> },
): Promise<Response> {
  return proxyJsonWrite(req, upstreamPath((await params).projectId), "PUT");
}

export async function PATCH(
  req: NextRequest,
  { params }: { params: Promise<{ projectId: string }> },
): Promise<Response> {
  return proxyJsonWrite(req, upstreamPath((await params).projectId), "PATCH");
}
