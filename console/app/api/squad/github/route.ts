// app/api/squad/github/route.ts — BFF squad GitHub-summary proxy (ISI-5705 WS-1b,
// consumed by the overview-UX redesign ISI-5697 / design ISI-5699).
//
// GET-ONLY. Proxies the apiserver's fleet-wide GitHub roll-up (GET /api/squad/github,
// githubsummary.go GithubSummary) — issue/PR/check totals, a 7-day merge sparkline, and a
// per-repo breakdown. Scoping is AUTHORITATIVE server-side (admin ⇒ every squad's repos; tenant
// ⇒ their own Team). The summary is sourced ENTIRELY from the already-synced scm mirror, never a
// live GitHub fan-out, so this path makes zero outbound GitHub calls. Response surfaced VERBATIM
// (401 / 404 / 501). No mutating verb is routed here → 405.

import type { NextRequest } from "next/server";
import { proxyJson } from "@/lib/bff";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

export async function GET(req: NextRequest): Promise<Response> {
  return proxyJson(req, "/api/squad/github");
}
