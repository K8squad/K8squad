// lib/project-files.ts — ISI-3956 S4c: the File Explorer payload types + the
// browser-side fetchers for the two BFF routes.
//
// Types mirror the S4b apiserver `/files` + `/files/content` contract (ADR-0012
// §D2, "reuse the buildbrowser Tree/File/Meta read-model shape"). v1 is
// READ-ONLY (ADR-0012 §D3): there is no write/exec/delete/rename shape here, on
// purpose. The tab reads the mount through the BFF choke point (browser → Next
// BFF → apiserver), never the apiserver directly, and the BFF relays upstream
// status VERBATIM so a 404 for an invisible project stays 404 (existence-hiding).
//
// "degraded" is the first-class "workspace busy → last-committed snapshot" state
// (ADR-0012 §RWO): the RWO PVC is held by a running agent, no co-mount, so S4b
// answers 200 with degraded=true and the bytes it can serve from the git
// snapshot. It is informational, never an error.

/** One entry in a directory listing. `type` is explicit (not inferred from a git
 * mode) so the tree renders dir-vs-file without parsing octal modes. `size` is
 * bytes for files (absent/0 for dirs). `path` is workspace-root-relative and is
 * what a lazy expand / preview re-requests. */
export type FileEntry = {
  name: string;
  path: string;
  type: "dir" | "file";
  size?: number;
};

/** GET /api/projects/{id}/files?path=<dir> response. `entries` arrives NULLABLE
 * on the wire (Go marshals a nil slice as `null`); normalize to `[]` everywhere.
 * `degraded` ⇒ the listing is the last-committed snapshot (workspace busy).
 * `truncated` ⇒ the directory had more entries than the server cap returned. */
export type FileListing = {
  path: string;
  entries: FileEntry[] | null;
  degraded?: boolean;
  // Labels the degraded shape (ISI-5140): "workspace_busy" when snapshot bytes
  // are actually being served (busy banner), "no_browse_target" for the honest
  // empty state. Must survive to the component — it drives which banner renders.
  reason?: string;
  truncated?: boolean;
  // RFC3339 instant the served snapshot was taken (ADR-0025 §7.2). Absent on
  // the wire today (spec §8 open point 1) — the freshness pill falls back to
  // the time the listing was fetched, never fabricating a timestamp.
  snapshotTakenAt?: string;
  // ADR-0025 D5a coherence token (ISI-5484): an opaque hash of (browse-target
  // Run UID, busy-bool) the apiserver stamps on every SERVED listing. The
  // client keys its durable per-session cache on it and HARD-INVALIDATES the
  // project's cached paths the instant it changes (busy↔idle flip or a new
  // succeeded run). Absent (omitempty) on a reader/apiserver that predates the
  // stamp → the entry is UNCACHEABLE (D5a M2), never keyed on "".
  generation?: string;
  // Client-stamped (never on the wire) when this listing was served SYNCHRONOUSLY
  // from the durable session cache (D5a cache-first render): `fromCache` lets the
  // component paint the tree with no blocking spinner and show the subtle
  // syncing…→updated/stale affordance; `cachedAt` is the ms epoch it was cached.
  fromCache?: boolean;
  cachedAt?: number;
};

/** GET /api/projects/{id}/files/content?path=<file> response. `data` is base64
 * (so binary bytes survive JSON transport); `contentType` is the S4b binary hint
 * — the console MUST NOT UTF-8-decode when it is "binary" (AC2). `size` is the
 * whole-file size; `offset`/`length` describe the returned window (byte-range /
 * size-cap, AC1). `truncated` ⇒ only a size-capped prefix was returned. */
export type FileContent = {
  path: string;
  size: number;
  contentType: "text" | "binary";
  data: string;
  offset: number;
  length: number;
  degraded?: boolean;
  truncated?: boolean;
};

/** GET /api/projects/{id}/files/stat?path=<file> response (ISI-4649). `git` is the
 * last-change commit for the path — ABSENT (omitempty) when the workspace is not a
 * git checkout or git is unavailable in the reader pod; that is a graceful no-git
 * fallback, never an error. `degraded` ⇒ snapshot vs live workspace (§RWO). */
export type FileStat = {
  name: string;
  type: "file" | "dir";
  size: number;
  /** RFC3339 filesystem mtime. */
  modTime: string;
  git?: FileGitChange;
  degraded?: boolean;
};

/** The most recent commit that touched a path (ISI-4649 GitChange). */
export type FileGitChange = {
  commitHash: string;
  author: string;
  message: string;
  /** RFC3339 commit time. */
  timestamp: string;
};

// Error taxonomy codes (ADR-0025 §taxonomy) — mirror of the Go ErrCode* constants.
export const FILE_ERR_RETRYABLE_DEGRADED = "retryable_degraded" as const;
export const FILE_ERR_SNAPSHOT_UNAVAILABLE = "snapshot_unavailable" as const;
export const FILE_ERR_PREPARING = "preparing" as const;
export const FILE_ERR_NOT_FOUND = "not_found" as const;

// Degraded-listing reason labels (ISI-5140, files.go DirListing.Reason) — the
// ADR-0025 §D2 vocabulary the six honest states bind to (ISI-5339 spec §5).
export const FILE_REASON_WORKSPACE_BUSY = "workspace_busy" as const;
export const FILE_REASON_NO_BROWSE_TARGET = "no_browse_target" as const;

/** Typed error body returned by the apiserver on 503/retryable errors (ADR-0025 §taxonomy). */
export type FileErrorBody = {
  error: string;
  code: string;
};

/** The distinct honest state an HTTP status carries (mirrors SquadOverview /
 *  GitHubStatusTab). 404 ⇒ existence-hiding not-found; 501 ⇒ the reader is not
 *  wired in this deployment (S4a/S4b pending) → "File Explorer not available yet",
 *  never fabricated rows (AC5).
 * `preparing` ⇒ HTTP 202 reader-warming (ADR-0025 D1/S2c): the reader pod
 * launched but hasn't passed healthz — the client polls with back-off.
 * `retrying` ⇒ transient 503 retryable-degraded (ADR-0025 D2); client is
 * auto-retrying with back-off. `attempt` is 1-based. */
export type FilesState<T> =
  | { kind: "loading" }
  | { kind: "preparing"; attempt: number }
  | { kind: "retrying"; attempt: number }
  | { kind: "unauthenticated" }
  | { kind: "not-found" }
  | { kind: "not-wired" }
  | { kind: "error"; status: number }
  | { kind: "ready"; data: T };

/** Map an HTTP status to the state it carries. */
export function classifyFilesStatus<T>(status: number): FilesState<T> {
  switch (status) {
    case 401:
      return { kind: "unauthenticated" };
    case 404:
      return { kind: "not-found" };
    case 501:
      return { kind: "not-wired" };
    default:
      return { kind: "error", status };
  }
}

// ADR-0025 D2: bounded auto-retry config for retryable-degraded (503) responses.
// ISI-5339 spec §4: the retry affordance is a 3-attempt terminal — the banner
// counts "Attempt {n} of RETRY_MAX_ATTEMPTS" and the terminal error panel takes
// over when the budget is exhausted.
export const RETRY_MAX_ATTEMPTS = 3;
const RETRY_BASE_MS = 2_000; // 2 s, 4 s, 8 s (capped at 16 s)

/** Compute the back-off delay for attempt `n` (1-based). */
export function retryDelayMs(attempt: number): number {
  return Math.min(RETRY_BASE_MS * 2 ** (attempt - 1), 16_000);
}

/** Whether a 503 response carries a retryable_degraded error code. */
async function isRetryableDegraded(res: Response): Promise<boolean> {
  if (res.status !== 503) return false;
  try {
    const body: FileErrorBody = await res.clone().json();
    return body.code === FILE_ERR_RETRYABLE_DEGRADED || body.code === FILE_ERR_PREPARING;
  } catch {
    return false;
  }
}

/** Whether a 2xx response is actually the 202 reader-warming body (ADR-0025
 * D1/S2c). MUST be checked BEFORE `res.ok`: 202 is inside the 2xx band, so the
 * ok-branch would otherwise try to read `{error, code}` as a listing and render
 * a fabricated-empty tree — exactly the bug the honest-states spec exists to
 * kill (ISI-5339 spec §2, "fabricated-empty"). */
function isPreparingStatus(status: number): boolean {
  return status === 202;
}

/** List a directory through the BFF choke point. `path` defaults to the
 * workspace root (""). Returns the classified state directly (200 ⇒ ready) so a
 * caller never fabricates rows on a non-200.
 * `signal` is forwarded to fetch so callers can cancel in-flight requests. */
export async function listProjectFiles(
  projectId: string,
  path = "",
  signal?: AbortSignal,
): Promise<FilesState<FileListing>> {
  const qs = path ? `?path=${encodeURIComponent(path)}` : "";
  const res = await fetch(
    `/api/projects/${encodeURIComponent(projectId)}/files${qs}`,
    { cache: "no-store", signal },
  );
  // 202 preparing (ADR-0025 D1/S2c) must be classified BEFORE the ok-branch.
  if (isPreparingStatus(res.status)) {
    return { kind: "preparing", attempt: 1 };
  }
  if (res.ok) {
    return { kind: "ready", data: normalizeListing(path, (await res.json()) as WireFileListing) };
  }
  // ADR-0025 D2: 503 retryable-degraded is signalled to the caller so the component
  // can show "retrying…" and schedule the next attempt — the fetcher itself does NOT
  // loop so that React state drives each visible transition.
  if (await isRetryableDegraded(res)) {
    return { kind: "retrying", attempt: 1 };
  }
  return classifyFilesStatus<FileListing>(res.status);
}

/** Variant used by the component retry loop: carries the current attempt number so
 * the state accurately reflects which attempt is in progress. */
export async function listProjectFilesAttempt(
  projectId: string,
  path: string,
  attempt: number,
  signal?: AbortSignal,
): Promise<FilesState<FileListing>> {
  const qs = path ? `?path=${encodeURIComponent(path)}` : "";
  const res = await fetch(
    `/api/projects/${encodeURIComponent(projectId)}/files${qs}`,
    { cache: "no-store", signal },
  );
  if (isPreparingStatus(res.status)) {
    // Preparing polls share the same bounded budget as retries (ISI-5339 §1:
    // "no row spins forever") — a reader that never warms surfaces the honest
    // terminal error instead of an endless spinning-up strip.
    if (attempt >= RETRY_MAX_ATTEMPTS) {
      return { kind: "error", status: 202 };
    }
    return { kind: "preparing", attempt: attempt + 1 };
  }
  if (res.ok) {
    return { kind: "ready", data: normalizeListing(path, (await res.json()) as WireFileListing) };
  }
  if (await isRetryableDegraded(res)) {
    if (attempt >= RETRY_MAX_ATTEMPTS) {
      return { kind: "error", status: 503 };
    }
    return { kind: "retrying", attempt: attempt + 1 };
  }
  return classifyFilesStatus<FileListing>(res.status);
}

/** The directory-listing shape as it ACTUALLY arrives from S4b (ISI-4705): the
 * reader/apiserver DirEntry is `{name,type,size}` — `path` is NOT on the Go
 * struct, so the wire type must model it as absent even though the rendered
 * `FileEntry` requires it. Typing the fetch boundary honestly is what removes
 * the `as unknown as` casts the tests otherwise need, and stops fixtures from
 * pretending the server sends a field it never has. */
export type WireFileEntry = Omit<FileEntry, "path"> & { path?: string };
export type WireFileListing = Omit<FileListing, "entries"> & { entries: WireFileEntry[] | null };

/** The content payload as it ACTUALLY arrives from S4b (ISI-4705): the reader
 * emits `{size,contentType,offset,length,data}` — `path` is NOT echoed, so the
 * wire type must model it as absent even though the rendered `FileContent` (and
 * the preview pane) require it. */
export type WireFileContent = Omit<FileContent, "path"> & { path?: string };

/** Give every listing entry a stable, unique workspace-root-relative `path`
 * (ISI-4705). The wire payload omits `path`; without it every `FileEntry.path`
 * is `undefined`, and the tree keys its per-directory open-state (`open`) and
 * lazy child listings (`dirs`) by that single shared `undefined` key — expanding
 * one directory flips EVERY directory to "open" over the same (root) listing and
 * renders recursively with no base case → stack overflow → the whole console
 * crashes. The path is derived LOCALLY from the requested directory + entry name
 * and the server's own `path` is intentionally NOT trusted: a POSIX name cannot
 * contain "/", so `${dir}/${name}` is unique by construction, and a server that
 * later emits a wrong or duplicated `path` (ISI-4708) still cannot reintroduce
 * the shared-key recursion. */
export function normalizeListing(dir: string, listing: WireFileListing): FileListing {
  const base = (dir ?? "").replace(/\/+$/, "");
  const entries: FileEntry[] = (listing.entries ?? []).map((e) => ({
    ...e,
    path: base ? `${base}/${e.name}` : e.name,
  }));
  return { ...listing, path: listing.path ?? dir, entries };
}

// ———————————————————————————————————————————————————————————————————————————
// ADR-0025 D5a — client durable listing cache (ISI-5485, child B).
//
// The shipped D5 server cache (ISI-5348) is a 300 ms per-session dedup — it
// CANNOT serve a cross-session revisit, and this client fetches `no-store`. So
// the durable cache-first layer lives here: a console-session Map that lets a
// revisited path render SYNCHRONOUSLY (no blocking spinner) while a non-blocking
// background revalidate reconciles the bytes.
//
// Coherence is keyed on the server `generation` stamp (child A, ISI-5484):
//   • cache ONLY `!degraded && !reason` listings that carry a non-empty
//     generation (M1: never cache a busy/degraded empty tree; M2: an absent
//     generation is UNCACHEABLE — never key on "", which would collide across
//     epochs during the mixed-version child-A rollout);
//   • a revalidation whose generation differs from the rendered entry's is NOT
//     reconciled in place across epochs — the project's cached paths are
//     HARD-INVALIDATED and the tree is REPLACED wholesale under the new
//     generation (C3: converge to the new epoch, never strand stale G1 bytes);
//   • a failed / degraded revalidate LEAVES the cache intact (AC3) — degraded,
//     not blank.
// ———————————————————————————————————————————————————————————————————————————

/** One durable cache entry: the clean (wire-stamped, never client-stamped)
 * listing plus the generation it was served under and the ms epoch it landed. */
type ListingCacheEntry = {
  generation: string;
  listing: FileListing;
  cachedAt: number;
};

// M3: an explicit LRU cap bounds cross-project growth — generation-invalidation
// alone only prunes a project on its own busy↔idle flip, so a user browsing many
// projects would otherwise grow the Map unbounded.
const LISTING_CACHE_MAX = 256;

// Map keyed by projectId path (NUL can't appear in either part), insertion
// order === recency: a get() re-inserts to mark MRU; a set() over the cap evicts
// the oldest. Module-scoped = one cache per console session (cleared on reload).
const listingCache = new Map<string, ListingCacheEntry>();

// M5: one in-flight revalidate per projectId path — the shipped 300 ms
// server dedup only collapses sub-300 ms bursts; this collapses the rest.
const inflightRevalidate = new Map<string, Promise<FileListing | null>>();

function cacheKey(projectId: string, path: string): string {
  return `${projectId} ${path}`;
}

/** Whether a listing may enter the durable cache (M1 + M2): never a degraded /
 * busy / no_browse_target tree, and only when the server stamped a non-empty
 * coherence generation. */
export function isCacheableListing(listing: FileListing): boolean {
  return !listing.degraded && !listing.reason && !!listing.generation;
}

/** Synchronous cache lookup for the cache-first render. Returns a client-stamped
 * clone (`fromCache:true` + the stored `cachedAt`) so the tree paints with no
 * blocking spinner, and touches LRU recency. `null` ⇒ cold path (unchanged). */
export function getCachedListing(projectId: string, path: string): FileListing | null {
  const key = cacheKey(projectId, path);
  const hit = listingCache.get(key);
  if (!hit) return null;
  // Mark MRU: delete + re-insert moves it to the tail of the iteration order.
  listingCache.delete(key);
  listingCache.set(key, hit);
  return { ...hit.listing, fromCache: true, cachedAt: hit.cachedAt };
}

/** Write-through a freshly-served listing. No-op (cache LEFT INTACT) when the
 * listing is uncacheable — a degraded/busy revalidation must never evict the
 * last good tree (AC3). Enforces the M3 LRU cap. */
export function cacheListing(projectId: string, path: string, listing: FileListing): void {
  if (!isCacheableListing(listing)) return;
  const key = cacheKey(projectId, path);
  // Strip any client stamps before storing so the cache holds only wire truth.
  const { fromCache: _f, cachedAt: _c, ...clean } = listing;
  listingCache.delete(key);
  listingCache.set(key, { generation: listing.generation as string, listing: clean, cachedAt: Date.now() });
  // Evict oldest (head of insertion order) until under the cap.
  while (listingCache.size > LISTING_CACHE_MAX) {
    const oldest = listingCache.keys().next().value as string | undefined;
    if (oldest === undefined) break;
    listingCache.delete(oldest);
  }
}

/** HARD-INVALIDATE every cached path for a project (C3: a generation change —
 * busy↔idle flip or a new succeeded run — means every cached path under the old
 * generation is now stale and must not survive). */
export function invalidateProjectListings(projectId: string): void {
  const prefix = `${projectId} `;
  for (const key of [...listingCache.keys()]) {
    if (key.startsWith(prefix)) listingCache.delete(key);
  }
}

/** Revalidate-only fetcher (M4): network stays `no-store` and reuses the abort
 * budget, but it NEVER escalates to the foreground preparing/retrying/terminal
 * state machine — a 202/503/4xx/5xx/abort all resolve to `null` so the caller
 * keeps the cached tree and shows the subtle stale affordance. Returns the fresh
 * listing ONLY on a 200 that is itself cacheable (`!degraded && generation`);
 * a 200-degraded (project went busy) also resolves `null` (keep cache, stale).
 * M5: collapses to a single in-flight request per projectId path. */
export function revalidateListing(
  projectId: string,
  path: string,
  signal?: AbortSignal,
): Promise<FileListing | null> {
  const key = cacheKey(projectId, path);
  const existing = inflightRevalidate.get(key);
  if (existing) return existing;
  const qs = path ? `?path=${encodeURIComponent(path)}` : "";
  const run = (async (): Promise<FileListing | null> => {
    try {
      const res = await fetch(
        `/api/projects/${encodeURIComponent(projectId)}/files${qs}`,
        { cache: "no-store", signal },
      );
      if (!res.ok) return null; // 202 preparing / 503 / 4xx / 5xx → keep cache, stale.
      const fresh = normalizeListing(path, (await res.json()) as WireFileListing);
      return isCacheableListing(fresh) ? fresh : null; // 200-degraded → keep cache, stale.
    } catch {
      return null; // abort / network error → keep cache, stale.
    } finally {
      inflightRevalidate.delete(key);
    }
  })();
  inflightRevalidate.set(key, run);
  return run;
}

/** The in-place reconcile of a rendered listing against fresh bytes (L1): match
 * rows by name+type, report what was added / removed / restated so the component
 * can prune the open/dirs child-state of removed subtrees and keep surviving
 * ones. `entries` is server-authoritative order + fresh stat. */
export type ListingReconcile = {
  entries: FileEntry[];
  added: string[];
  removed: string[];
  restated: string[];
};

function rowKey(e: FileEntry): string {
  return `${e.type} ${e.name}`;
}

export function reconcileListing(prev: FileEntry[], next: FileEntry[]): ListingReconcile {
  const prevByKey = new Map(prev.map((e) => [rowKey(e), e] as const));
  const nextByKey = new Map(next.map((e) => [rowKey(e), e] as const));
  const added: string[] = [];
  const restated: string[] = [];
  for (const e of next) {
    const was = prevByKey.get(rowKey(e));
    if (!was) added.push(e.path);
    else if (was.size !== e.size) restated.push(e.path);
  }
  const removed: string[] = [];
  for (const e of prev) {
    if (!nextByKey.has(rowKey(e))) removed.push(e.path);
  }
  return { entries: next, added, removed, restated };
}

/** The coherence verdict for a background revalidation (C3). Given the currently
 * RENDERED listing and the `revalidateListing` result, decides whether to show
 * the cache as stale, reconcile fresh same-generation bytes in place, or replace
 * the whole subtree under a new generation. Owns the cache + invalidation writes
 * so the component only applies the UI transition. */
export type RevalidationOutcome =
  | { kind: "stale" }
  | { kind: "updated"; listing: FileListing; reconcile: ListingReconcile }
  | { kind: "replaced"; listing: FileListing };

export function applyRevalidation(
  projectId: string,
  path: string,
  rendered: FileListing,
  fresh: FileListing | null,
): RevalidationOutcome {
  // No fresh bytes (failed / degraded / aborted) → cache already untouched (AC3).
  if (!fresh) return { kind: "stale" };
  const renderedGen = rendered.generation;
  // C3: generation changed → the rendered epoch is gone. Hard-invalidate the
  // whole project's cache, cache the new entry, and REPLACE wholesale (never a
  // per-row reconcile across epochs — that would leave stale G1 rows alive).
  if (renderedGen && fresh.generation !== renderedGen) {
    invalidateProjectListings(projectId);
    cacheListing(projectId, path, fresh);
    return { kind: "replaced", listing: fresh };
  }
  // Same generation → refresh the cache and reconcile the rows in place (L1).
  cacheListing(projectId, path, fresh);
  return { kind: "updated", listing: fresh, reconcile: reconcileListing(rendered.entries ?? [], fresh.entries ?? []) };
}

/** Test-only: drop all durable cache + in-flight state between cases. */
export function __resetListingCache(): void {
  listingCache.clear();
  inflightRevalidate.clear();
}

/** Read a file's content through the BFF choke point. Read-only — there is no
 * write counterpart by design (§D3). `signal` is forwarded to fetch. */
export async function readProjectFile(
  projectId: string,
  path: string,
  signal?: AbortSignal,
): Promise<FilesState<FileContent>> {
  const res = await fetch(
    `/api/projects/${encodeURIComponent(projectId)}/files/content?path=${encodeURIComponent(path)}`,
    { cache: "no-store", signal },
  );
  if (isPreparingStatus(res.status)) {
    return { kind: "preparing", attempt: 1 };
  }
  if (res.ok) {
    return { kind: "ready", data: normalizeContent(path, (await res.json()) as FileContent) };
  }
  return classifyFilesStatus<FileContent>(res.status);
}

/** Guarantee a content payload carries the `path` it was requested for (ISI-4705).
 * The S4b `/files/content` wire payload is `{size,contentType,offset,length,data}`
 * — it does NOT echo `path` (same omission as the listing). `FileContent.path` is
 * declared required and the preview pane reaches for it immediately
 * (`previewKind(c.path,…)` → `fileExt(c.path)` → `path.slice(…)`), so an undefined
 * `path` threw `Cannot read properties of undefined (reading 'slice')` the instant a
 * file was opened. The caller always knows the exact path it fetched — use it as the
 * authoritative value. */
export function normalizeContent(path: string, content: WireFileContent): FileContent {
  return { ...content, path: content.path && content.path.length > 0 ? content.path : path };
}

/** Fetch a file's stat / change metadata through the BFF choke point (ISI-4651).
 * Read-only, same classified-state contract as list/read — a stat failure never
 * fabricates details. */
export async function statProjectFile(
  projectId: string,
  path: string,
): Promise<FilesState<FileStat>> {
  const res = await fetch(
    `/api/projects/${encodeURIComponent(projectId)}/files/stat?path=${encodeURIComponent(path)}`,
    { cache: "no-store" },
  );
  if (isPreparingStatus(res.status)) {
    return { kind: "preparing", attempt: 1 };
  }
  if (res.ok) {
    return { kind: "ready", data: (await res.json()) as FileStat };
  }
  return classifyFilesStatus<FileStat>(res.status);
}

/** Build the BFF download URL for a workspace path (ISI-4652). A file path
 * streams an octet-stream attachment; a directory path streams a server-built
 * tar.gz archive (ISI-4650). Used as a plain `<a href download>` — the session
 * cookie rides the same-origin request, so no fetch/blob handling is needed and
 * upstream errors (404/413/501/503) surface as the browser's native download
 * failure rather than fabricated UI state. Read-only: this is a GET, no write
 * path into the volume (§D3). */
export function downloadProjectFileUrl(projectId: string, path: string): string {
  return `/api/projects/${encodeURIComponent(projectId)}/files/download?path=${encodeURIComponent(path)}`;
}

/** Decode a base64 text payload to a UTF-8 string. Goes through bytes (not a
 * bare `atob`) so multibyte UTF-8 survives — `atob` yields latin1 code units,
 * which would mojibake any non-ASCII source file. Callers guard on
 * contentType==="text" first; this is never called for binary (AC2). */
export function decodeTextContent(data: string): string {
  const binary = atob(data);
  const bytes = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i++) bytes[i] = binary.charCodeAt(i);
  return new TextDecoder().decode(bytes);
}

/** Humanize a byte count for the binary placeholder + size hints (AC2). */
export function humanBytes(n: number | undefined): string {
  if (n == null || Number.isNaN(n)) return "unknown size";
  if (n < 1024) return `${n} B`;
  const units = ["KB", "MB", "GB", "TB"];
  let v = n / 1024;
  let u = 0;
  while (v >= 1024 && u < units.length - 1) {
    v /= 1024;
    u++;
  }
  return `${v.toFixed(v < 10 ? 1 : 0)} ${units[u]}`;
}

/** A `data:` URL for the read-only "download raw bytes" affordance on a binary
 * file (AC2). Read-only: this hands back exactly the bytes S4b served, no write
 * path into the volume. */
export function rawBytesDataUrl(content: FileContent): string {
  return `data:application/octet-stream;base64,${content.data}`;
}

/** Preview classification for the type-aware viewer (ISI-4648, per the validated
 * ISI-4602 mocks: go/node/md/json/yaml highlighted, md rendered, svg/png shown
 * as pictures, everything else binary gets the honest placeholder). The
 * classification is EXTENSION-FIRST: the S4b `contentType` binary hint alone
 * cannot tell a renderable PNG from a `.so`, nor markdown from plain text. */
export type FilePreviewKind = "image" | "markdown" | "code" | "text" | "binary";

/** Classify a fetched file for the preview pane. Extension decides image vs
 * markdown vs code; the wire binary hint then guards everything else (AC2 —
 * never UTF-8-decode binary bytes). */
export function previewKind(path: string, contentType: "text" | "binary"): FilePreviewKind {
  const ext = fileExt(path);
  if (ext === "png" || ext === "svg") return "image";
  if (contentType === "binary") return "binary";
  if (ext === "md" || ext === "markdown") return "markdown";
  if (codeLanguage(path) !== null) return "code";
  return "text";
}

/** The highlight.js language id for a path, or null when the file previews as
 * plain text. Covers the mock set: go, node (js/ts), json, yaml. */
export function codeLanguage(path: string): string | null {
  switch (fileExt(path)) {
    case "go":
      return "go";
    case "ts":
    case "tsx":
      return "typescript";
    case "js":
    case "jsx":
    case "mjs":
    case "cjs":
      return "javascript";
    case "json":
      return "json";
    case "yaml":
    case "yml":
      return "yaml";
    default:
      return null;
  }
}

/** A `data:` URL that renders an image preview (svg/png) from the bytes S4b
 * served. Works whether the server hinted the payload text or binary — `data`
 * is base64 on the wire either way. Read-only: hands back exactly the served
 * bytes, no write path into the volume. */
export function imageDataUrl(content: FileContent): string {
  const mime = fileExt(content.path) === "svg" ? "image/svg+xml" : "image/png";
  return `data:${mime};base64,${content.data}`;
}

function fileExt(path: string): string {
  // Defense-in-depth (ISI-4705): never throw on a missing path. The wire types
  // declare `path` required but the server omits it, and a bare `undefined.slice`
  // here is exactly what crashed the preview. Callers normalize the path, but this
  // guarantees the extension helpers degrade to "no extension" instead of throwing.
  if (!path) return "";
  const name = path.slice(path.lastIndexOf("/") + 1);
  const dot = name.lastIndexOf(".");
  return dot > 0 ? name.slice(dot + 1).toLowerCase() : "";
}
