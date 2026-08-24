// app/api/otelconfig/route.ts — BFF proxy for the OTLP exporter config (story 8.12).
//
// The browser talks ONLY to this Next.js server; reads and writes are proxied to the Go
// apiserver's /api/otelconfig surface, which composes/applies the `OTelConfig` CRD (1.5)
// through the ONE authorization choke point (§13/ADR-013). The BFF adds no second authz path
// and no client-side authz — statuses surface VERBATIM. A 404 on GET is the OPT-IN default
// (no OTelConfig exists → no exporter → telemetry stays in-cluster), which the form renders
// as the empty state, not an error. The apiserver-side CRD store + reconciler wiring is
// 13.8/ISI-2917; until it lands a 404 exercises exactly this default-state path.

import type { NextRequest } from "next/server";
import { proxyJson, proxyJsonWrite } from "@/lib/bff";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

export async function GET(req: NextRequest): Promise<Response> {
  return proxyJson(req, "/api/otelconfig");
}

export async function PUT(req: NextRequest): Promise<Response> {
  return proxyJsonWrite(req, "/api/otelconfig", "PUT");
}
