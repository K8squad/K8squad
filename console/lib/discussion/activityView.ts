// Activity-view cleaner (ISI-5696) — the pure projection behind the discussion
// Room's "demote the raw run-activity stream" ask. The live `thinking` envelopes
// (lib/discussion/liveFeed.ts#ThinkingRow) carry the per-run EXECUTION stream the
// ISI-4792/4798 run-detail DEBUG screen was built for — `[run <id>]` scope
// markers, `[tool:<name>/result(ok)]` call/observation markers, `[server:…]`
// transport tags, `[untrusted]` intermediate reasoning, and `[status] …`
// lifecycle lines. Rendered verbatim inline (the pre-ISI-5696 ThinkingStream),
// that scaffolding drowns the one thing a party-room reader wants: each agent's
// posted reply, which lands separately as its own message bubble.
//
// This module strips the bracketed execution prefixes off a thinking body so the
// collapsed "Run activity" affordance reads as plain progress text instead of
// wire markers, leaning on run-activity.ts#parseRunScope for the `[run …]` token
// (the same stripping the run-detail classifier applies). It is transport-free
// and renders nothing itself — ThinkingStream folds these rows behind a
// collapsed toggle so none of this is visible in the default transcript.

import { parseRunScope } from "@/lib/run-activity";

// A leading run of bracketed execution markers: `[tool:…]`, `[server:…]`,
// `[untrusted]`, `[status]` (and any whitespace between them). `[^\]]*` stops at
// the closing bracket, so a `[tool:bash/result(ok)]` marker — slashes and parens
// included — is consumed whole. Matched greedily from the front only; a bracket
// appearing later in real prose is left untouched.
const LEADING_EXEC_MARKERS =
  /^(?:\[(?:tool:[^\]]*|server:[^\]]*|untrusted|status[^\]]*)\]\s*)+/i;

/**
 * Strip the `[run <id>]` scope prefix and any leading execution markers off a
 * live thinking body, returning readable progress text. When stripping would
 * leave nothing (a bare `[status]`/`[tool:…]` marker with no trailing text), the
 * run-scope-stripped remainder is returned instead so a row never renders blank.
 */
export function cleanActivityBody(body: string): string {
  const { rest } = parseRunScope(body ?? "");
  const stripped = rest.replace(LEADING_EXEC_MARKERS, "").trim();
  return stripped || rest.trim();
}
