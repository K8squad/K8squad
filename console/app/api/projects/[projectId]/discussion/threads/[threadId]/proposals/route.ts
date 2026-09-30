// app/api/projects/[projectId]/discussion/threads/[threadId]/proposals/route.ts — BFF proxy for a
// thread's action-proposal cards (ISI-4928 create / ISI-4930 read; client seam
// lib/discussion/api.ts listProposals + postProposal).
//
//   GET  — the thread's proposal cards joined with lifecycle phase (durable card state after a
//          reload). The room loads this on open, so a missing handler surfaced as a console 404.
//   POST — propose an inert action card; provenance of the proposer is server-stamped and the
//          payload names the authorizable action. The body is relayed UNCHANGED.
//
// Proxied to the Go apiserver's discussion handler (internal/discussion/handler.go), which owns the
// AUTHORITATIVE deny-by-default authZ gate (§13 / ADR-013): the session identity is forwarded and
// the apiserver status surfaced VERBATIM — a cross-tenant or absent thread is a 404 (existence-
// hiding, NEVER re-mapped to 403). Without this route Next.js had no handler for the segment and
// answered 404 even though the apiserver route exists — a BFF proxy gap (class of ISI-5164),
// fixed by ISI-5250. PUT/PATCH/DELETE → 405.

import type { NextRequest } from "next/server";
import { proxyJson, proxyJsonWrite } from "@/lib/bff";
import { encodeProjectId } from "@/lib/projectId";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

export async function GET(
  req: NextRequest,
  { params }: { params: Promise<{ projectId: string; threadId: string }> },
): Promise<Response> {
  const { projectId: p, threadId: t } = await params;
  const projectId = encodeProjectId(p);
  const threadId = encodeURIComponent(t);
  return proxyJson(
    req,
    `/api/projects/${projectId}/discussion/threads/${threadId}/proposals`,
  );
}

export async function POST(
  req: NextRequest,
  { params }: { params: Promise<{ projectId: string; threadId: string }> },
): Promise<Response> {
  const { projectId: p, threadId: t } = await params;
  const projectId = encodeProjectId(p);
  const threadId = encodeURIComponent(t);
  return proxyJsonWrite(
    req,
    `/api/projects/${projectId}/discussion/threads/${threadId}/proposals`,
    "POST",
  );
}
