// app/api/modelendpoints/list-models/route.ts — BFF proxy for the model-listing probe
// (ISI-5005). POST {provider, url?, apiKey?} → {models:[{id,label?}]}.
//
// The apiserver — NOT the browser — dials the provider (an Ollama-native /api/tags or an
// OpenAI-compatible /models), so the (optional) API key never leaves the server side and a LAN
// Ollama endpoint unreachable from the user's browser can still be probed from inside the
// cluster. Admin-tier authz + endpoint validation live in the apiserver; the BFF forwards the
// caller's session and body UNCHANGED and relays the response VERBATIM (422 validation, 403
// non-admin, honest 5xx on an upstream failure).
//
// This POST dials a caller-chosen endpoint with a caller-supplied key, so it is gated
// same-origin FIRST (crossSiteReject) before any upstream call — a cross-site form must not be
// able to make the apiserver dial an attacker endpoint under the victim's session.

import type { NextRequest } from "next/server";
import { crossSiteReject, proxyJsonWrite } from "@/lib/bff";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

export async function POST(req: NextRequest): Promise<Response> {
  const rejected = crossSiteReject(req);
  if (rejected) return rejected;
  return proxyJsonWrite(req, "/api/modelendpoints/list-models", "POST");
}
