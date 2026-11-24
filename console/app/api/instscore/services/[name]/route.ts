// app/api/instscore/services/[name]/route.ts — BFF proxy for one service's
// instrumentation-score detail (ISI-7750, phase 4 of the ISI-7741 plan).
//
// GET-ONLY. The service name is forwarded percent-encoded as a single path
// segment (the same discipline as the Team org proxy); a 404 from the apiserver
// (service never observed) relays verbatim — absence is a plain not-found.

import type { NextRequest } from "next/server";
import { proxyJson } from "@/lib/bff";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

export async function GET(
  req: NextRequest,
  { params }: { params: Promise<{ name: string }> },
): Promise<Response> {
  const name = encodeURIComponent((await params).name);
  return proxyJson(req, `/api/v1/instscore/services/${name}`);
}
