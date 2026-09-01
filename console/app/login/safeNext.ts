// app/login/safeNext.ts — post-login redirect sanitizer for the /login route (ISI-3522).
//
// Lives OUTSIDE page.tsx on purpose: a Next.js App Router `page.tsx` may only export the default
// component plus Next's reserved exports (metadata / route-segment config). An arbitrary named
// export (`sanitizeNext`) makes `next build` reject the file with "does not match the required types
// of a Next.js Page" — so the pure, testable helper is co-located here instead.

// sanitizeNext keeps the post-login redirect on THIS origin only — an open-redirect guard. We accept
// a root-relative path ("/…") and reject anything the browser could resolve OFF-origin: a scheme
// URL, protocol-relative "//host", or the backslash variant "/\host" — browsers normalize "\" to
// "/", so `/\evil.example` resolves as `https://evil.example/` (Copilot review of PR #215). After the
// cheap rejects we resolve against our own origin and require the result to STAY same-origin, so any
// exotic authority that slips through the string checks is caught by the URL parser.
export function sanitizeNext(raw: string | null, origin: string): string {
  if (!raw) return "/";
  if (!raw.startsWith("/")) return "/"; // must be root-relative (no scheme, no bare host)
  if (raw.includes("\\")) return "/"; // "\" == "/" to the browser → "/\evil" is protocol-relative
  if (raw.startsWith("//")) return "/"; // protocol-relative → off-origin
  try {
    const url = new URL(raw, origin);
    if (url.origin !== origin) return "/"; // defense in depth: authority survived → reject
    return url.pathname + url.search + url.hash;
  } catch {
    return "/";
  }
}
