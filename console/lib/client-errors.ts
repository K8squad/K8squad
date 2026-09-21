// lib/client-errors.ts — client-side crash/error reporting (ISI-4705).
//
// The console had NO client telemetry: when a render throw or a runaway
// main-thread render took the UI down ("file explorer crash"), there was
// nothing to collect — no console log the user could grab after the tab died,
// no server-side record. This is the seam that makes a client crash
// COLLECTABLE, per the O11y principle that data must drive diagnosis.
//
// Two channels, both best-effort and NEVER allowed to throw (a reporter that
// throws would compound the very crash it reports):
//   1. a structured, greppable `console.error("[client-crash]", …)` — visible
//      in browser devtools and captured by RUM console-forwarding once wired.
//   2. a `navigator.sendBeacon` to the BFF sink (`/api/telemetry/client-error`)
//      so the crash is recorded server-side (console pod stderr → the same log
//      pipeline as every other component) even when the tab is dying.

/** The structured crash record. Kept small and PII-light: a source tag, the
 * error message + stack, and an optional bag of non-sensitive context (route,
 * projectId, the file path being previewed, byte sizes). */
export type ClientErrorReport = {
  source: string;
  message: string;
  stack?: string;
  context?: Record<string, unknown>;
  at: string;
  url?: string;
  userAgent?: string;
};

const BEACON_PATH = "/api/telemetry/client-error";

/** Report a client-side error through both channels. Safe to call from a React
 * error boundary's componentDidCatch, a window.onerror handler, or a catch
 * block. Returns nothing and swallows every failure of its own. */
export function reportClientError(
  source: string,
  err: unknown,
  context?: Record<string, unknown>,
): void {
  try {
    const report: ClientErrorReport = {
      source,
      message: err instanceof Error ? err.message : String(err),
      stack: err instanceof Error ? err.stack : undefined,
      context,
      at: new Date().toISOString(),
      url: typeof location !== "undefined" ? location.pathname + location.search : undefined,
      userAgent: typeof navigator !== "undefined" ? navigator.userAgent : undefined,
    };

    // Channel 1: structured console — greppable tag, always emitted.
    // eslint-disable-next-line no-console
    console.error("[client-crash]", report);

    // Channel 2: best-effort server beacon (fire-and-forget; a 404/blocked
    // beacon just returns false — never throws, never blocks the crash path).
    if (typeof navigator !== "undefined" && typeof navigator.sendBeacon === "function") {
      const blob = new Blob([JSON.stringify(report)], { type: "application/json" });
      navigator.sendBeacon(BEACON_PATH, blob);
    }
  } catch {
    // A reporter must never throw. If even console.error/Blob is unavailable,
    // there is nothing more we can safely do.
  }
}
