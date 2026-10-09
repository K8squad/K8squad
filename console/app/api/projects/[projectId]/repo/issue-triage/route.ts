// app/api/projects/[projectId]/repo/issue-triage/route.ts — BFF proxy for the
// ISI-5595 WS-E issue auto-triage config sub-resource (GET read, PUT/PATCH write).
//
// The browser talks ONLY to this Next.js server; the request is proxied to the Go
// apiserver's issue-triage service (issuetriage.go), which owns the AUTHORITATIVE
// authZ gate (§12.3 deny-by-default: read = member+, write = contributor+) and
// server-stamps the provenance (enabledBy) + forward-only watermark (enabledAt),
// NEVER trusting a body-supplied value. The apiserver is the wall; this adds NO
// second authz path and asserts NO principal (§13 / ADR-013). It is the DEDICATED
// sub-resource path, a sibling of review-automation — deliberately NOT bolted onto
// the /github status route.
//
// The BFF forwards the caller's session cookie and surfaces the apiserver's status
// VERBATIM — a deny is existence-hiding (404 stays 404), a 422 (enabled⇒agent)
// stays 422, and the documented 501 (service not wired in a cluster-less dev run)
// stays 501 so the dialog renders its honest "not available yet" state.

import type { NextRequest } from "next/server";
import { proxyJson, proxyJsonWrite } from "@/lib/bff";
import { encodeProjectId } from "@/lib/projectId";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

// upstreamPath collapses the Project id to EXACTLY one encoding layer (ISI-3982 /
// ISI-4662 double-encode trap) — the same discipline review-automation uses.
function upstreamPath(projectId: string): string {
  return `/api/projects/${encodeProjectId(projectId)}/repo/issue-triage`;
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
