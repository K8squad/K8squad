// lib/revalidate-telemetry.ts — ISI-5500 [S5a-C-fu2]: the D5a file-revalidate
// beacon emitter (the telemetry signal source the ISI-5486 obs design handed to
// child B, omitted by PR#787). Modeled EXACTLY on lib/client-errors.ts
// (`reportClientError`, ISI-4705): a best-effort `navigator.sendBeacon` POST to a
// LOCAL BFF sink route (no apiserver proxy) that writes one structured JSON line
// to the console pod's stderr → the shared log pipeline → queryable in Dynatrace.
//
// Why a client beacon and not a server counter (ISI-5486 §1-2): the four D5a
// signals (cache-hit serve, background-revalidate outcome, dropped-stale, hard-
// invalidate) are inherently CLIENT-SIDE — a cache hit makes NO network request,
// and a drop/invalidate is a post-response client decision the apiserver cannot
// observe. So the signal source must be the console and the transport a
// client→server beacon.
//
// Field contract is ISI-5486 §3 (VERBATIM). PII/cardinality firewall (§8): the
// beacon NEVER carries the path string or file names — only `pathDepth` (int),
// a bounded opaque `projectId`, and `genHashPrefix` (first 6 hex of the opaque
// generation hash, which is itself hash(RunUID, busy) so no raw id leaks).
//
// Hard rule: a telemetry failure must NEVER throw into the render path — the
// whole point of D5a is degraded-tolerance, and a broken beacon must not break
// it. Every emit is try/catch-swallowed and guarded on sendBeacon existing.

/** The local BFF sink route (§5) — same-origin, cookie rides along, never a
 * data-bearing apiserver route. */
export const REVALIDATE_TELEMETRY_URL = "/api/telemetry/file-revalidate";

// Head-based 1-in-N sampler for the HIGH-VOLUME events (every `serve`, and
// `revalidate` success). Failures + invalidates are rare and alert-bearing, so
// they are ALWAYS sent (never sampled). One-line tunable (ISI-5486 §5): start
// N=1 in staging to validate the transport end-to-end, bump to 10 in prod. The
// chosen N is echoed as `sampleRate` in every body so DQL can scale counts back
// up (`sum(sampleRate)`), including on the always-sent events (sampleRate=1).
export const REVALIDATE_SAMPLE_N = 1;

/** Terminal outcome of a background revalidate (ISI-5486 §3). `success` = fresh
 * bytes applied (reconcile in place, or a wholesale replace under a new
 * generation); `dropped_stale_generation` = response generation != rendered
 * generation and the response was dropped; `failed` = network/HTTP/abort, the
 * cached tree was kept with no takeover. */
export type RevalidateOutcome = "success" | "dropped_stale_generation" | "failed";

/** REQUIRED reason on `outcome:"failed"` (ISI-5486 §3) — reuses the S1 taxonomy
 * verbatim (project-files.ts FILE_ERR_* / FILE_REASON_*) plus the two transport
 * reasons the classifier can't see. */
export type RevalidateFailReason =
  | "retryable_degraded"
  | "snapshot_unavailable"
  | "preparing"
  | "not_found"
  | "workspace_busy"
  | "no_browse_target"
  | "network"
  | "aborted";

/** Cause of a hard-invalidate (ISI-5486 §3). The shipped coherence guard cannot
 * distinguish a busy↔idle flip from a new browse-target — both surface as a
 * generation change — so it reports the generic `generation_changed`; the §6
 * Tile-4 DQL coalesces to exactly this label. */
export type InvalidateReason = "busy_flip" | "new_browse_target" | "generation_changed";

/** Integer depth of a workspace path — NOT the path string (§8 PII firewall).
 * "" (root) → 0, "a/b/c" → 3. */
function pathDepth(path: string): number {
  if (!path) return 0;
  return path.split("/").filter(Boolean).length;
}

/** First 6 hex of the opaque generation token (§8): correlates a client event to
 * the files.go response that stamped it, never the Run UID. Absent generation →
 * undefined (omitted from the body). */
function genHashPrefix(generation: string | undefined): string | undefined {
  if (!generation) return undefined;
  return generation.slice(0, 6);
}

/** 1-in-N head-based decision. N<=1 always sends. */
function sampled(): boolean {
  if (REVALIDATE_SAMPLE_N <= 1) return true;
  return Math.floor(Math.random() * REVALIDATE_SAMPLE_N) === 0;
}

/** The single best-effort transport. Guarded on sendBeacon existing (absent in
 * SSR / node test env / old browsers), try/catch-swallowed, NEVER throws. */
function send(body: Record<string, unknown>): void {
  try {
    if (typeof navigator === "undefined" || typeof navigator.sendBeacon !== "function") return;
    const blob = new Blob([JSON.stringify(body)], { type: "application/json" });
    navigator.sendBeacon(REVALIDATE_TELEMETRY_URL, blob);
  } catch {
    // Best-effort telemetry — a beacon failure must never surface into render.
  }
}

/** `serve` seam (ISI-5486 §3): emitted at render time. `fromCache:true` = served
 * synchronously from the client cache (the stale-serve case); `revisit:true` =
 * this projectId|path was rendered earlier this session. Sampled 1-in-N. */
export function emitServe(opts: {
  projectId: string;
  path: string;
  fromCache: boolean;
  revisit: boolean;
  generation?: string;
}): void {
  if (!sampled()) return;
  send({
    tag: "file-revalidate",
    event: "serve",
    fromCache: opts.fromCache,
    revisit: opts.revisit,
    projectId: opts.projectId,
    pathDepth: pathDepth(opts.path),
    genHashPrefix: genHashPrefix(opts.generation),
    sampleRate: REVALIDATE_SAMPLE_N,
  });
}

/** `revalidate` seam (ISI-5486 §3): emitted when the background fetch resolves or
 * drops. `success` is sampled 1-in-N like serve; every non-success is ALWAYS sent
 * (rare + alert-bearing). `reason` is REQUIRED on `failed`. `latencyMs` is the
 * cache-render → reconcile/drop/fail duration; `generationChanged` is true when
 * the round crossed a generation boundary (coherence guard fired). */
export function emitRevalidate(opts: {
  projectId: string;
  path: string;
  outcome: RevalidateOutcome;
  reason?: RevalidateFailReason;
  latencyMs: number;
  generationChanged: boolean;
  generation?: string;
}): void {
  // Always send failures/drops; sample only the high-volume success case.
  if (opts.outcome === "success" && !sampled()) return;
  send({
    tag: "file-revalidate",
    event: "revalidate",
    outcome: opts.outcome,
    reason: opts.reason,
    latencyMs: opts.latencyMs,
    generationChanged: opts.generationChanged,
    projectId: opts.projectId,
    pathDepth: pathDepth(opts.path),
    genHashPrefix: genHashPrefix(opts.generation),
    sampleRate: REVALIDATE_SAMPLE_N,
  });
}

/** `invalidate` seam (ISI-5486 §3): emitted when the coherence guard hard-evicts
 * a project's cached paths on a generation change. ALWAYS sent (never sampled) —
 * it is the cleanest confirmation the guard fired, and `pathsEvicted` makes the
 * project-coarse blast radius visible. */
export function emitInvalidate(opts: {
  projectId: string;
  reason: InvalidateReason;
  pathsEvicted: number;
}): void {
  send({
    tag: "file-revalidate",
    event: "invalidate",
    reason: opts.reason,
    pathsEvicted: opts.pathsEvicted,
    projectId: opts.projectId,
    sampleRate: 1,
  });
}
