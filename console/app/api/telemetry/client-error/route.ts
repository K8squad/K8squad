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
// data-bearing route. Two abuse controls (PR #530 review):
//   - crossSiteReject (the house `/api/*` idiom, lib/bff.ts): `/api/*` is public
//     because each BFF handler owns its own guard; a write handler that acts
//     without forwarding identity must still refuse cross-site callers.
//   - a byte cap enforced from the Content-Length header BEFORE the body is
//     read into memory (and re-checked on the decoded bytes), so a large POST
//     can neither flood the log pipeline nor balloon the pod's heap.

import type { NextRequest } from "next/server";
import { crossSiteReject } from "@/lib/bff";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

// A crash report (message + stack + small context) is a few KiB at most; cap
// well above that but far below anything that could flood logs or the heap.
const MAX_BODY_BYTES = 16 * 1024;

export async function POST(req: NextRequest): Promise<Response> {
  // Refuse cross-site callers up front — this handler acts without forwarding a
  // session, so it must not be a public, forgeable write into the log pipeline.
  const rejected = crossSiteReject(req);
  if (rejected) return rejected;

  try {
    // Cap BEFORE reading the body into memory: a declared oversize length is
    // refused without ever materialising the payload (a bare req.text() would
    // buffer the whole thing first — the DoS this guard exists to prevent).
    const declared = Number(req.headers.get("content-length") ?? 0);
    if (Number.isFinite(declared) && declared > MAX_BODY_BYTES) {
      return new Response(null, { status: 413 });
    }
    const raw = await req.text();
    // Re-check on real bytes (Buffer.byteLength, not String.length which counts
    // UTF-16 code units) in case Content-Length was absent or understated.
    if (Buffer.byteLength(raw, "utf8") > MAX_BODY_BYTES) {
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
