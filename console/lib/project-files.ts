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
    return { kind: "ready", data: (await res.json()) as FileListing };
  }
  return classifyFilesStatus<FileListing>(res.status);
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
    return { kind: "ready", data: (await res.json()) as FileContent };
  }
  return classifyFilesStatus<FileContent>(res.status);
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
