// app/api/projects/[projectId]/repo/ci-automation/route.ts — BFF proxy for the
// ISI-5595 WS-E Actions CI-failure triage config sub-resource (GET read, PUT/PATCH
// write).
//
// Proxies to the Go apiserver's ci-failure service (cifailure.go), which owns the
// AUTHORITATIVE authZ gate (§12.3 deny-by-default: read = member+, write =
// contributor+) and server-stamps enabledBy + the forward-only enabledAt
// watermark. The apiserver is the wall; this BFF adds NO second authz path and
// asserts NO principal (§13 / ADR-013), forwarding the caller's session cookie and
// surfacing the apiserver status VERBATIM (404 existence-hiding, 422 bad
// conclusions/enabled⇒agent, 501 not wired → the dialog's honest states).

import type { NextRequest } from "next/server";
import { proxyJson, proxyJsonWrite } from "@/lib/bff";
import { encodeProjectId } from "@/lib/projectId";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

// upstreamPath collapses the Project id to EXACTLY one encoding layer (ISI-3982 /
// ISI-4662 double-encode trap).
function upstreamPath(projectId: string): string {
  return `/api/projects/${encodeProjectId(projectId)}/repo/ci-automation`;
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
