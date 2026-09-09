// lib/projectId.ts — the ONE place the console normalizes a Project id as it
// crosses the URL boundary (ISI-3982).
//
// A Project id is a Kubernetes "namespace/name" (e.g. "bmad-squad/bmad-demo-project").
// The slash means it is percent-encoded to a SINGLE path segment ("ns%2Fname") in
// every `/projects/[projectId]/...` URL. Next.js hands a route/page the still-encoded
// segment for ids that contain an encoded slash, so the naïve `encodeURIComponent`
// at each hop re-encoded an already-encoded id — page redirect, BFF proxy, and the
// browser client each added a layer until the apiserver's mux saw a triple-mangled id
// and 404'd (ISI-3982 root cause).
//
// The rule enforced here, applied at EVERY boundary:
//   • param-consuming pages/layouts  → decodeProjectId(params.projectId)  (canonical "ns/name")
//   • URL-building sites (client + BFF) → encodeProjectId(id)             (exactly one layer)
//
// Both helpers are idempotent for our id shape (a DNS-label ns/name carries no literal
// '%'), so calling them on an already-normalized value is a safe no-op.

/**
 * Decode a `[projectId]` route param to its canonical "namespace/name" form.
 * Accepts either the still-encoded segment ("ns%2Fname") or an already-decoded
 * value ("ns/name") and returns the decoded id. A malformed escape passes through
 * unchanged rather than throwing, so a bad URL renders an honest empty state
 * instead of a 500.
 */
export function decodeProjectId(raw: string): string {
  try {
    return decodeURIComponent(raw);
  } catch {
    return raw;
  }
}

/**
 * Collapse a Project id to EXACTLY one layer of percent-encoding for an upstream
 * URL, regardless of whether the caller/router handed us the decoded id or the
 * still-encoded path segment. This is the "decode-then-encode once, deliberately"
 * pattern: it fixes the double/triple-encode by first stripping the encoding the
 * router preserved, then applying a single, canonical layer.
 */
export function encodeProjectId(raw: string): string {
  return encodeURIComponent(decodeProjectId(raw));
}
