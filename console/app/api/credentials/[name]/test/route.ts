// app/api/credentials/[name]/test/route.ts — BFF test-connection proxy (E3-S2 / ISI-3680, AD-7).
//
// POST-ONLY. Relays the caller's session to the apiserver's stateless probe
// (POST /api/credentials/{name}/test) VERBATIM — the probe runs server-side
// against the STORED managed credential (the client NEVER re-sends the value;
// the request body carries routing hints only: {runtime, modelEndpointRef}).
// The apiserver answers {ok, detail} and caches the last result as Team
// annotations (AD-2) so the Launchpad reflects it on resume. Status codes
// surface verbatim: 200 {ok,detail}, 401 unauthenticated, 404 no-such-
// credential (existence-hiding), 422 bad runtime hint, 501 human-seat /
// service-not-wired. `name` is percent-encoded so it cannot inject an
// arbitrary upstream path.

import type { NextRequest } from "next/server";
import { crossSiteReject, proxyJsonWrite } from "@/lib/bff";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

export async function POST(
  req: NextRequest,
  { params }: { params: Promise<{ name: string }> },
): Promise<Response> {
  // Same-origin gate first: the probe is a POST that acts on the caller's session (ISI-3983).
  const rejected = crossSiteReject(req);
  if (rejected) return rejected;
  const { name } = await params;
  return proxyJsonWrite(
    req,
    `/api/credentials/${encodeURIComponent(name)}/test`,
    "POST",
  );
}
