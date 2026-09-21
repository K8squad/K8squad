// app/api/telemetry/client-error/route.ts — client crash sink (ISI-4705).
//
// POST-ONLY. Receives the structured crash reports emitted by
// lib/client-errors.ts (`reportClientError`) — typically via navigator.sendBeacon
// as the tab is dying — and writes them to the console pod's stderr as one
// structured JSON line tagged `client-crash`. That is enough to make a
// previously-invisible client crash COLLECTABLE: the console pod's logs flow
// through the same pipeline as every other component, so the report is
// queryable alongside the server-side traces of the same session.
//
// This is deliberately a LOCAL sink (no apiserver proxy): a crash report is
// low-trust, unauthenticated telemetry and must never be able to reach a
// data-bearing route. The body is hard-capped so a beacon can't be used to
// flood the log pipeline, and every field is treated as untrusted text.

import type { NextRequest } from "next/server";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

// A crash report (message + stack + small context) is a few KiB at most; cap
// well above that but far below anything that could flood logs.
const MAX_BODY_BYTES = 16 * 1024;

export async function POST(req: NextRequest): Promise<Response> {
  try {
    const raw = await req.text();
    if (raw.length > MAX_BODY_BYTES) {
      return new Response(null, { status: 413 });
    }
    let report: unknown;
    try {
      report = JSON.parse(raw);
    } catch {
      return new Response(null, { status: 400 });
    }
    // One structured line → console pod stderr → the shared log pipeline.
    // eslint-disable-next-line no-console
    console.error(
      JSON.stringify({ tag: "client-crash", receivedAt: new Date().toISOString(), report }),
    );
  } catch {
    // Never surface a 5xx to the beacon path — a crash reporter that itself
    // 500s would just be noise. Swallow and 204.
  }
  return new Response(null, { status: 204 });
}
