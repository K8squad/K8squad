// app/api/credentials/connect/route.ts — BFF Connect-Claude seam (story 8.6 → 7.7 / ISI-2899).
//
// POST-only. The one-click browser-OAuth flow lives in the Go apiserver (it must: it mints the
// login URL, takes the callback, and writes the OAuth tokens into the per-user Secret — the
// console never touches token strings). The BFF relays the caller's session and the apiserver's
// answer VERBATIM; today that answer is the documented 501 naming ISI-2899, which the screen
// renders as the button's legible "not configured yet" state — an honest contract, never a
// fabricated login.
//
// ⚠ CSRF posture (PR #87 review, forward-looking): the moment ISI-2899 turns this into a real
// cookie-authenticated state-changing OAuth initiation, the flow MUST gain an Origin/Referer
// check (or SameSite=Strict on the session cookie) — proxyJsonWrite adds none today. Harmless
// while the endpoint answers 501; required before the flow goes live. Tracked with ISI-2899.

import type { NextRequest } from "next/server";
import { proxyJsonWrite } from "@/lib/bff";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

export async function POST(req: NextRequest): Promise<Response> {
  return proxyJsonWrite(req, "/api/credentials/connect", "POST");
}
