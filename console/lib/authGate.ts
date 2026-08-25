// lib/authGate.ts — the pure decision behind the Next.js middleware (Epic 15.5 route protection, ISI-2921).
//
// ARCHITECTURAL BOUNDARY (§13 / ADR-013): this is NOT an authorization layer. The Go apiserver is the
// ONE authz choke point — it mints the internal JWT and applies the §12.3 deny-by-default RBAC wall
// (and the 15.4 per-Project membership gate). The BFF (lib/bff.ts) forwards the caller's identity and
// surfaces the apiserver's decision verbatim; it adds NO second authz path.
//
// This gate is strictly a ROUTING convenience: it keeps an anonymous browser (no session cookie at all)
// from loading the authenticated app shell, redirecting it to /login instead. It NEVER decodes, trusts,
// or role-checks the cookie — a present-but-invalid/expired cookie passes this gate and is rejected
// upstream by the apiserver (fail-closed there, existence-hiding preserved). Cookie presence is a hint,
// not a grant. Keeping the real decision upstream is what stops a client-forgeable authz path.

/** Route prefixes that must render WITHOUT a session (else an anonymous user could never reach login). */
const PUBLIC_PREFIXES = ["/login", "/_next", "/favicon", "/public", "/assets"] as const;

/** Exact public files at the root. */
const PUBLIC_FILES = ["/favicon.ico", "/robots.txt", "/sitemap.xml", "/manifest.webmanifest"] as const;

/**
 * isPublicPath reports whether a path is reachable without a session. /api/* is public HERE because the
 * BFF route handlers own their own auth (they forward the cookie and surface the apiserver's status —
 * an unauthenticated API call must get a 401 JSON, never an HTML login redirect that would corrupt a
 * fetch/JSON client).
 */
export function isPublicPath(pathname: string): boolean {
  if (pathname.startsWith("/api/")) return true;
  for (const p of PUBLIC_PREFIXES) if (pathname === p || pathname.startsWith(p + "/")) return true;
  for (const f of PUBLIC_FILES) if (pathname === f) return true;
  return false;
}

export interface GateResult {
  /** "next" ⇒ allow the request through; "redirect" ⇒ send the browser to `location`. */
  action: "next" | "redirect";
  location?: string;
}

/**
 * gateDecision is the pure middleware decision. `hasSession` is whether the request carries a
 * non-empty session cookie (presence only — the caller extracts it; this function never sees the value).
 * On an anonymous navigation to a protected route it redirects to /login with a `next` param so the
 * login flow can return the user where they were headed.
 */
export function gateDecision(pathname: string, search: string, hasSession: boolean): GateResult {
  if (isPublicPath(pathname) || hasSession) return { action: "next" };
  // Anonymous → login. Preserve the intended destination (path + query) for post-login return.
  const dest = pathname + (search || "");
  const next = encodeURIComponent(dest);
  return { action: "redirect", location: `/login?next=${next}` };
}
