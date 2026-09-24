// app/api/modelconfig/route.ts — BFF proxy for the org-default model tier read
// (ISI-4890, epic ISI-4822 Flow A).
//
// GET /api/modelconfig hydrates the Settings→Configuration Model Priority section
// from the apiserver, which reads the well-known ModelConfig/default singleton
// (k8squad-system/default — the floor of the ISI-4430 resolution ladder) through
// the ONE authz choke point (§13/ADR-013). The browser talks ONLY to this Next.js
// server; statuses surface VERBATIM. A 404 is the OPT-IN empty-form state (no org
// default configured yet), mirroring the OTLP config surface — the section renders
// it as an empty form under the fail-closed guardrail, not an error.
//
// The WRITE half is the generic compose route: POST /api/compose/modelconfig →
// apiserver POST /api/modelconfig (upsert). This file is the read half only.

import type { NextRequest } from "next/server";
import { proxyJson } from "@/lib/bff";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

export async function GET(req: NextRequest): Promise<Response> {
  return proxyJson(req, "/api/modelconfig");
}
