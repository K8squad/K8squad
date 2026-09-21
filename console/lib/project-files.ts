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
  truncated?: boolean;
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

/** The distinct honest state an HTTP status carries (mirrors SquadOverview /
 * GitHubStatusTab). 404 ⇒ existence-hiding not-found; 501 ⇒ the reader is not
 * wired in this deployment (S4a/S4b pending) → "File Explorer not available yet",
 * never fabricated rows (AC5). */
export type FilesState<T> =
  | { kind: "loading" }
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

/** List a directory through the BFF choke point. `path` defaults to the
 * workspace root (""). Returns the classified state directly (200 ⇒ ready) so a
 * caller never fabricates rows on a non-200. */
export async function listProjectFiles(
  projectId: string,
  path = "",
): Promise<FilesState<FileListing>> {
  const qs = path ? `?path=${encodeURIComponent(path)}` : "";
  const res = await fetch(
    `/api/projects/${encodeURIComponent(projectId)}/files${qs}`,
    { cache: "no-store" },
  );
  if (res.ok) {
    return { kind: "ready", data: normalizeListing(path, (await res.json()) as WireFileListing) };
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

/** Read a file's content through the BFF choke point. Read-only — there is no
 * write counterpart by design (§D3). */
export async function readProjectFile(
  projectId: string,
  path: string,
): Promise<FilesState<FileContent>> {
  const res = await fetch(
    `/api/projects/${encodeURIComponent(projectId)}/files/content?path=${encodeURIComponent(path)}`,
    { cache: "no-store" },
  );
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
