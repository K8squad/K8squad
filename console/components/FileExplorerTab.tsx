"use client";

// components/FileExplorerTab.tsx — ISI-3956 S4c: the Project File Explorer tab.
//
// A file TREE (lazy-expanding directories) + a read-only PREVIEW pane, fed by the
// S4b `/files` + `/files/content` routes through the BFF choke point. v1 is
// READ-ONLY (ADR-0012 §D3): there is deliberately NO edit / save / delete /
// rename / run affordance anywhere in this component.
//
// Every honest state the story pins is a first-class render:
//   loading (AC3) · binary placeholder, no garbled text (AC2) ·
//   workspace-busy/degraded banner over the last-committed snapshot (AC4) ·
//   empty "no files yet" + 501 "not available yet" + 404 existence-hiding (AC5).
//
// The tree expands lazily: each directory node fetches its own listing the first
// time it is opened (GET .../files?path=), so a deep workspace never front-loads
// the whole tree. Selecting a file fetches its content (GET .../files/content).

import { Component, type ReactNode, useCallback, useEffect, useMemo, useRef, useState } from "react";
import hljs from "highlight.js/lib/core";
import hlGo from "highlight.js/lib/languages/go";
import hlTypescript from "highlight.js/lib/languages/typescript";
import hlJavascript from "highlight.js/lib/languages/javascript";
import hlJson from "highlight.js/lib/languages/json";
import hlYaml from "highlight.js/lib/languages/yaml";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import "./file-explorer.css";
import { EmptyState } from "@/components/forms/EmptyState";
import { reportClientError } from "@/lib/client-errors";
import {
  listProjectFiles,
  listProjectFilesAttempt,
  readProjectFile,
  statProjectFile,
  downloadProjectFileUrl,
  decodeTextContent,
  humanBytes,
  rawBytesDataUrl,
  previewKind,
  codeLanguage,
  imageDataUrl,
  retryDelayMs,
  FILE_REASON_NO_BROWSE_TARGET,
  FILE_REASON_WORKSPACE_BUSY,
  RETRY_MAX_ATTEMPTS,
  type FileEntry,
  type FileContent,
  type FileStat,
  type FilesState,
} from "@/lib/project-files";

// ISI-4648: type-aware viewer grammars, registered once on the core build so
// the bundle carries only the mock set (go/node/json/yaml), not every hljs
// language.
hljs.registerLanguage("go", hlGo);
hljs.registerLanguage("typescript", hlTypescript);
hljs.registerLanguage("javascript", hlJavascript);
hljs.registerLanguage("json", hlJson);
hljs.registerLanguage("yaml", hlYaml);

/** Per-directory lazy-load state, keyed by the directory's workspace-relative
 * path ("" = root). A directory is fetched the first time it is expanded. */
type DirState = FilesState<{ entries: FileEntry[]; degraded?: boolean; reason?: string; snapshotTakenAt?: string }>;

// ISI-4705: the largest byte window the rich renderers (highlight.js code view,
// ReactMarkdown) will process on the main thread. A capped read is up to 1 MiB
// (readserver maxReadBytes); syntax-highlighting or markdown-parsing that much —
// especially a minified single-line file — blocks the main thread for seconds
// and can OOM-crash the tab. Above this cap the preview degrades to an honest
// "too large to preview — download" panel. Plain text (<pre>) and the binary
// placeholder are cheap and unaffected.
const RICH_PREVIEW_MAX_BYTES = 256 * 1024;

// ISI-4705 defense-in-depth: `TreeNode` recurses on `entry.path` with no cycle
// detection, so ANY listing with a repeating/duplicate path (a future server bug,
// a symlink loop) re-enters it forever. normalizeListing keys on a locally-unique
// `${dir}/${name}`, but a bounded recursion depth is the belt to that suspenders:
// past this depth a directory renders an honest "too deep" row instead of
// recursing, so the tree can never stack-overflow the console again.
const MAX_TREE_DEPTH = 64;

// ISI-4747: the File Explorer tree icon set. The live tree had drifted to emoji /
// arrow glyphs (📄 for files, a bare ▸/▾ expand arrow with NO folder icon, ⬇ for
// download) — far from the mocks. These are stroke SVGs on the same 24-grid and
// currentColor vocabulary as the nav rail (NavIcon), so a directory reads as a
// real folder (closed → open), a file as a document, and the caret is a rotating
// chevron. Single-path, multi-subpath `d` strings (Feather/Lucide geometry).
const TREE_ICON = {
  chevron: "M9 18l6-6-6-6",
  folder: "M22 19a2 2 0 0 1-2 2H4a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h5l2 3h9a2 2 0 0 1 2 2z",
  folderOpen:
    "M6 14l1.5-2.9A2 2 0 0 1 9.24 10H20a2 2 0 0 1 1.94 2.5l-1.55 6a2 2 0 0 1-1.94 1.5H4a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h3.9a2 2 0 0 1 1.69.9l.81 1.2a2 2 0 0 0 1.67.9H18a2 2 0 0 1 2 2v2",
  file: "M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z M14 2v6h6 M16 13H8 M16 17H8 M10 9H8",
  download: "M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4 M7 10l5 5 5-5 M12 15V3",
  // ISI-4747 (follow-up): per-type file glyphs. Distinct silhouettes per content
  // family so the eye can sort code from data from an image at a glance — the
  // same 24-grid / currentColor vocabulary as the folder glyphs above.
  code: "M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z M14 2v6h6 M9.5 12.5L7.5 14.5l2 2 M14.5 12.5l2 2-2 2",
  data: "M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z M14 2v6h6 M9.5 12a1.5 1.5 0 0 0-1.5 1.5 1.5 1.5 0 0 1-1 1.4 1.5 1.5 0 0 1 1 1.4A1.5 1.5 0 0 0 9.5 19 M14.5 12a1.5 1.5 0 0 1 1.5 1.5 1.5 1.5 0 0 0 1 1.4 1.5 1.5 0 0 0-1 1.4 1.5 1.5 0 0 1-1.5 1.5",
  image: "M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z M14 2v6h6 M9 13a1 1 0 1 0 0-2 1 1 0 0 0 0 2 M20 19l-4-4-3 3-2-2-3 3",
  media: "M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z M14 2v6h6 M10 12.5v5l4-2.5z",
  archive: "M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z M14 2v6h6 M11 12h2 M11 15h2 M11 18h2",
  // ISI-5339 (S6): state-frame glyphs for the six honest states + the freshness
  // pill — same 24-grid / currentColor vocabulary as the tree icons above.
  clock: "M12 22a10 10 0 1 0 0-20 10 10 0 0 0 0 20z M12 6v6l4 2",
  refresh: "M23 4v6h-6 M1 20v-6h6 M3.51 9a9 9 0 0 1 14.85-3.36L23 10 M1 14l4.64 4.36A9 9 0 0 0 20.49 15",
  lock: "M5 11h14v10H5z M8 11V7a4 4 0 0 1 8 0v4",
  info: "M12 22a10 10 0 1 0 0-20 10 10 0 0 0 0 20z M12 16v-4 M12 8h.01",
  box: "M21 8l-9-5-9 5v8l9 5 9-5z M3 8l9 5 9-5 M12 13v8",
  alert: "M12 22a10 10 0 1 0 0-20 10 10 0 0 0 0 20z M12 8v4 M12 16h.01",
} as const;

/** One tree glyph. A single <path> renders every subpath in the `d` string, so
 * multi-part icons (file lines, folder-open lid) need no per-subpath elements. */
function TreeIcon({ name, className }: { name: keyof typeof TREE_ICON; className?: string }) {
  return (
    <svg
      className={className}
      width="16"
      height="16"
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.7"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
    >
      <path d={TREE_ICON[name]} />
    </svg>
  );
}

// ISI-4747 (follow-up, board request): map a file name to a type glyph + a tone
// token. The glyph is the silhouette (code / data / image / media / archive /
// doc); the tone drives the hue in file-explorer.css so languages that share the
// `code` glyph — go, py, java, c#, … — still read apart at a glance (the GitHub
// language-colour model, kept monochrome-per-icon). Unknown extensions fall back
// to the plain document glyph in the muted tone, so nothing ever loses an icon.
const FILE_TYPE: Record<string, { icon: keyof typeof TREE_ICON; tone: string }> = {};
{
  const reg = (icon: keyof typeof TREE_ICON, tone: string, exts: string[]) => {
    for (const e of exts) FILE_TYPE[e] = { icon, tone };
  };
  // Languages — shared `code` glyph, distinct tone per language.
  reg("code", "go", ["go"]);
  reg("code", "py", ["py", "pyi", "pyw"]);
  reg("code", "js", ["js", "mjs", "cjs", "jsx"]);
  reg("code", "ts", ["ts", "tsx", "mts", "cts"]);
  reg("code", "java", ["java", "jar"]);
  reg("code", "cs", ["cs"]);
  reg("code", "rust", ["rs"]);
  reg("code", "ruby", ["rb", "erb", "gemspec"]);
  reg("code", "php", ["php"]);
  reg("code", "cpp", ["c", "h", "cpp", "cc", "cxx", "hpp", "hh"]);
  reg("code", "shell", ["sh", "bash", "zsh", "fish", "ps1"]);
  reg("code", "web", ["html", "htm", "css", "scss", "sass", "less", "vue", "svelte"]);
  reg("code", "code", ["swift", "kt", "kts", "scala", "lua", "r", "dart", "ex", "exs", "clj", "pl", "sql", "graphql"]);
  // Structured data / config — braces glyph.
  reg("data", "data", ["json", "yaml", "yml", "toml", "ini", "cfg", "conf", "env", "xml", "csv", "tsv", "proto", "lock"]);
  // Plain text & lightweight markup — document glyph, muted.
  reg("file", "doc", ["md", "mdx", "markdown", "txt", "rst", "adoc", "tex", "log", "text"]);
  // Rich office documents & PDF — document glyph, warm tone.
  reg("file", "docx", ["doc", "docx", "odt", "rtf", "pdf", "ppt", "pptx", "xls", "xlsx", "ods"]);
  // Images — picture glyph.
  reg("image", "image", ["png", "jpg", "jpeg", "gif", "svg", "webp", "bmp", "ico", "avif", "tif", "tiff"]);
  // Audio / video — play glyph.
  reg("media", "media", ["mp4", "mov", "webm", "mkv", "avi", "mp3", "wav", "flac", "ogg", "m4a", "aac"]);
  // Archives — box-with-slats glyph.
  reg("archive", "archive", ["zip", "tar", "gz", "tgz", "bz2", "xz", "rar", "7z", "zst"]);
}

/** Resolve a file name to its type glyph + tone. Dotfiles with no extension
 * (`.gitignore`) and unknown extensions fall back to the plain document glyph. */
function fileTypeGlyph(name: string): { icon: keyof typeof TREE_ICON; tone: string } {
  const dot = name.lastIndexOf(".");
  const ext = dot > 0 ? name.slice(dot + 1).toLowerCase() : "";
  return FILE_TYPE[ext] ?? { icon: "file", tone: "default" };
}

// ——— ISI-5339 (S6): Frame 01/02/03 progressive-loading + honest-state pieces ———
// Zero new design tokens: every hue is an existing status token
// (--status-running / --status-paused / --status-idle / --status-blocked /
// --accent) and every radius an existing --radius-* (spec §"reskin only").

/** A coarse relative age for the freshness pill ("just now", "12s ago"). */
function relativeAge(ms: number): string {
  if (ms < 10_000) return "just now";
  const s = Math.floor(ms / 1000);
  if (s < 60) return `${s}s ago`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ago`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h ago`;
  return `${Math.floor(h / 24)}d ago`;
}

/** Frame 03·A — the Live/Snapshot freshness pill for the Files header. A trust
 * signal, never an alarm: Live = green dot + pulse on the live PVC read;
 * Snapshot = amber clock over a point-in-time snapshot, with the absolute
 * timestamp on hover (`snapshotTakenAt` when the wire carries it, else the time
 * the listing was fetched). The pulse ring honours prefers-reduced-motion
 * (CSS below renders a static dot). */
function FreshnessPill({
  mode,
  fetchedAt,
  snapshotTakenAt,
}: {
  mode: "live" | "snapshot";
  fetchedAt: number;
  snapshotTakenAt?: string;
}) {
  // Freshness updates while you browse (spec §3): a slow 10 s tick is enough —
  // the label is coarse ("12s ago"), not a stopwatch.
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), 10_000);
    return () => clearInterval(t);
  }, []);
  const parsed = snapshotTakenAt ? Date.parse(snapshotTakenAt) : NaN;
  const anchor = Number.isNaN(parsed) ? fetchedAt : parsed;
  const abs = new Date(anchor).toLocaleString();
  if (mode === "live") {
    return (
      <span
        className="file-explorer__pill file-explorer__pill--live"
        data-testid="files-pill-live"
        title={`Live workspace — refreshed ${abs}`}
      >
        <span className="file-explorer__pill-dot" aria-hidden="true" />
        Live · {relativeAge(now - fetchedAt)}
      </span>
    );
  }
  return (
    <span
      className="file-explorer__pill file-explorer__pill--snapshot"
      data-testid="files-pill-snapshot"
      title={`Snapshot taken ${abs}`}
    >
      <TreeIcon name="clock" className="file-explorer__pill-clock" />
      Snapshot · {relativeAge(now - anchor)}
    </span>
  );
}

/** Frame 01 — skeleton tree rows (muted rounded bars) while a listing is in
 * flight. `aria-hidden` bars; the surrounding region carries the single
 * aria-busy "Loading files…" label (spec §6). */
function SkeletonRows({ depth, count = 5 }: { depth: number; count?: number }) {
  return (
    <>
      {Array.from({ length: count }, (_, i) => (
        <li key={i} aria-hidden="true">
          <div
            className={`file-explorer__skeleton file-explorer__skeleton--w${(i % 3) + 1}`}
            style={{ marginLeft: 8 + depth * 14 }}
            data-testid="files-skeleton"
          />
        </li>
      ))}
    </>
  );
}

/** Frame 01 — the bounded "spinning up" strip (blue): spin glyph + honest copy
 * + a determinate-FEELING progress bar keyed to the bounded poll budget, not an
 * indeterminate spinner that reads as hung (spec §1). Driven by the 202
 * preparing contract; no retryAfterMs on the wire so progress is attempt-based
 * (spec §8 open point 2 fallback). */
function SpinningUpStrip({ attempt }: { attempt: number }) {
  const pct = Math.min(100, Math.round((attempt / RETRY_MAX_ATTEMPTS) * 100));
  return (
    <div className="file-explorer__spinup" role="status" data-testid="files-preparing">
      <TreeIcon name="refresh" className="file-explorer__spin file-explorer__spin--accent" />
      <div className="file-explorer__spinup-copy">
        <p className="file-explorer__spinup-title">Spinning up the file reader…</p>
        <p className="muted">
          Getting a read-only view of this workspace ready. This usually takes a few seconds.
        </p>
        <div className="file-explorer__progress" aria-hidden="true">
          <div className="file-explorer__progress-fill" style={{ width: `${pct}%` }} />
        </div>
      </div>
    </div>
  );
}

/** Frame 03·B — the amber read-timeout banner: auto-retry with a visible
 * attempt budget + a [Retry now] that refetches immediately and keeps the
 * user's place in the tree (spec §4). role="status": polite, expected. */
function RetryBanner({ attempt, onRetryNow }: { attempt: number; onRetryNow: () => void }) {
  const seconds = Math.round(retryDelayMs(attempt) / 1000);
  return (
    <div className="banner banner--warn" role="status" data-testid="files-retrying">
      <p className="file-explorer__banner-title">
        <TreeIcon name="refresh" className="file-explorer__spin" /> This is taking longer than
        usual — retrying…
      </p>
      <p className="muted">
        We&apos;re retrying automatically — attempt {attempt} of {RETRY_MAX_ATTEMPTS}, next try in{" "}
        {seconds}s. You can wait or retry now; your place in the tree is kept.
      </p>
      <button type="button" className="btn" onClick={onRetryNow} data-testid="files-retry-now">
        Retry now
      </button>
    </div>
  );
}

/** Frame 02 — one honest state frame: badge (top-right) + framed icon + title
 * + body, each state visually distinct so fabricated-empty, genuinely-empty,
 * and busy never look alike (spec §2). */
function StateFrame({
  tone,
  badge,
  icon,
  title,
  body,
  testId,
  action,
  alert,
  detail,
}: {
  tone: "accent" | "amber" | "slate" | "rose";
  badge: string;
  icon: keyof typeof TREE_ICON;
  title: string;
  body: string;
  testId: string;
  action?: ReactNode;
  alert?: boolean;
  detail?: string;
}) {
  return (
    <div
      className={`file-explorer__state file-explorer__state--${tone}`}
      role={alert ? "alert" : "status"}
      data-testid={testId}
      title={detail}
    >
      <span className={`file-explorer__state-badge file-explorer__state-badge--${tone}`}>
        {badge}
      </span>
      <div className={`file-explorer__state-frame file-explorer__state-frame--${tone}`}>
        <TreeIcon name={icon} className="file-explorer__state-icon" />
      </div>
      <h2 className="file-explorer__state-title">{title}</h2>
      <p className="muted file-explorer__state-body">{body}</p>
      {action}
    </div>
  );
}

/** A reporting error boundary around the File Explorer subtree (ISI-4705). The
 * files route had NO boundary, so any render throw — a bad payload, a renderer
 * choking on a large file — propagated to the root and white-screened the whole
 * console ("the ui crash"). This contains the throw to an honest, recoverable
 * panel AND reports it (lib/client-errors) so the crash is collectable instead
 * of vanishing with the tab. */
export class FileExplorerErrorBoundary extends Component<
  { projectId: string; children: ReactNode },
  { failed: boolean }
> {
  constructor(props: { projectId: string; children: ReactNode }) {
    super(props);
    this.state = { failed: false };
  }

  static getDerivedStateFromError() {
    return { failed: true };
  }

  componentDidCatch(error: unknown, info: { componentStack?: string }) {
    reportClientError("file-explorer", error, {
      projectId: this.props.projectId,
      componentStack: info?.componentStack,
    });
  }

  render() {
    if (this.state.failed) {
      return honest(
        "File Explorer hit a problem",
        "Something went wrong rendering this workspace. The rest of the console is unaffected — reload this tab to try again.",
      );
    }
    return this.props.children;
  }
}

/** The File Explorer tab, wrapped in its reporting error boundary. */
export function FileExplorerTab({ projectId }: { projectId: string }) {
  // `key={projectId}` remounts the boundary on a project change so a crash in
  // one project does not latch its error panel over the next project's healthy
  // Files tab (the App Router reuses this instance across the [projectId] param).
  return (
    <FileExplorerErrorBoundary key={projectId} projectId={projectId}>
      <FileExplorerTabInner projectId={projectId} />
    </FileExplorerErrorBoundary>
  );
}

function FileExplorerTabInner({ projectId }: { projectId: string }) {
  // Root listing drives the top-level tree + the terminal honest states
  // (unauth / not-found / not-wired / error / empty) for the whole tab.
  const [root, setRoot] = useState<DirState>({ kind: "loading" });
  // Expanded directory paths → their loaded (or loading) listing state.
  const [dirs, setDirs] = useState<Record<string, DirState>>({});
  // The set of currently-open directory paths (drives the caret + child render).
  const [open, setOpen] = useState<Set<string>>(() => new Set());
  // The selected file preview (path + fetched content state).
  const [selected, setSelected] = useState<string | null>(null);
  const [preview, setPreview] = useState<FilesState<FileContent>>({ kind: "loading" });
  // ISI-4651: the selected file's stat / change metadata (details pane).
  const [stat, setStat] = useState<FilesState<FileStat>>({ kind: "loading" });
  // ISI-5339 (S6): [Retry now] / [Try again] bump this to cancel any scheduled
  // back-off timer and refetch the root immediately (tree position preserved).
  const [rootTick, setRootTick] = useState(0);
  // The pending auto-retry timer, kept cancellable for Retry now.
  const retryTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  // When the current root listing landed — drives the Live freshness label.
  const [rootFetchedAt, setRootFetchedAt] = useState<number | null>(null);

  const refreshRoot = useCallback(() => {
    if (retryTimer.current) {
      clearTimeout(retryTimer.current);
      retryTimer.current = null;
    }
    setRootTick((t) => t + 1);
  }, []);

  const loadDir = useCallback(
    async (path: string, signal?: AbortSignal): Promise<DirState> => {
      const state = await listProjectFiles(projectId, path, signal);
      if (state.kind !== "ready") return state;
      return {
        kind: "ready",
        // ISI-5140: preserve `reason` — the render logic (busyDegraded / noTarget)
        // keys the workspace-busy banner on reason==="workspace_busy"; dropping it
        // here left busyDegraded permanently false, so the banner never rendered.
        data: { entries: state.data.entries ?? [], degraded: state.data.degraded, reason: state.data.reason, snapshotTakenAt: state.data.snapshotTakenAt },
      };
    },
    [projectId],
  );

  // Load the root listing once (and on project change / Retry now).
  // ADR-0025 D2: AbortController cancels the in-flight request on unmount / project
  // change; retrying/preparing states schedule a bounded back-off retry
  // (ISI-5339 §1: no row — and no strip — spins forever).
  useEffect(() => {
    let alive = true;
    const ac = new AbortController();
    setRoot({ kind: "loading" });
    setDirs({});
    setOpen(new Set());
    setSelected(null);
    setRootFetchedAt(null);

    async function fetchRoot(attempt: number, isRetry = false) {
      try {
        // ISI-5339 (S6): only the INITIAL fetch goes through loadDir; every
        // scheduled retry/preparing poll uses listProjectFilesAttempt so the
        // attempt budget actually escalates (the old code keyed on
        // `attempt === 1`, so every retry re-entered the first-fetch path and
        // the bounded 3-attempt terminal could never fire — it retried
        // "attempt 1" forever).
        const s = !isRetry
          ? await loadDir("", ac.signal)
          : await (async (): Promise<DirState> => {
              const raw = await listProjectFilesAttempt(projectId, "", attempt, ac.signal);
              if (raw.kind !== "ready") return raw;
                return {
                  kind: "ready",
                  data: { entries: raw.data.entries ?? [], degraded: raw.data.degraded, reason: raw.data.reason, snapshotTakenAt: raw.data.snapshotTakenAt },
                };
            })();
        if (!alive) return;
        if (s.kind === "retrying" || s.kind === "preparing") {
          setRoot(s);
          const delay = retryDelayMs(s.attempt);
          retryTimer.current = setTimeout(() => {
            if (alive) void fetchRoot(s.attempt, true);
          }, delay);
        } else {
          if (s.kind === "ready") setRootFetchedAt(Date.now());
          setRoot(s);
        }
      } catch {
        if (alive) setRoot({ kind: "error", status: 0 });
      }
    }

    void fetchRoot(1);
    return () => {
      alive = false;
      ac.abort();
      if (retryTimer.current) {
        clearTimeout(retryTimer.current);
        retryTimer.current = null;
      }
    };
  }, [loadDir, projectId, rootTick]);

  const toggleDir = useCallback(
    (path: string) => {
      setOpen((prev) => {
        const next = new Set(prev);
        if (next.has(path)) {
          next.delete(path);
          return next;
        }
        next.add(path);
        // Lazy-load the first time this directory is opened.
        setDirs((cur) => {
          if (cur[path]) return cur;
          void loadDir(path)
            .then((s) => setDirs((d) => ({ ...d, [path]: s })))
            .catch(() => setDirs((d) => ({ ...d, [path]: { kind: "error", status: 0 } })));
          return { ...cur, [path]: { kind: "loading" } };
        });
        return next;
      });
    },
    [loadDir],
  );

  const selectFile = useCallback(
    (path: string) => {
      setSelected(path);
      setPreview({ kind: "loading" });
      readProjectFile(projectId, path)
        .then(setPreview)
        .catch(() => setPreview({ kind: "error", status: 0 }));
      setStat({ kind: "loading" });
      statProjectFile(projectId, path)
        .then(setStat)
        .catch(() => setStat({ kind: "error", status: 0 }));
    },
    [projectId],
  );

  // ——— progressive / honest states for the whole tab (root listing) ———
  // ISI-5339 Frame 01: loading / preparing / retrying NEVER blank the tab —
  // the shell paints instantly with skeleton rows (plus the bounded spinning-up
  // strip on 202 preparing, or the amber retry banner on read-timeout).
  if (root.kind === "loading" || root.kind === "preparing" || root.kind === "retrying") {
    return (
      <section
        aria-busy="true"
        aria-label="Loading files…"
        data-testid={root.kind === "loading" ? "files-loading" : undefined}
      >
        <header>
          <h1>File Explorer</h1>
          <p className="muted">Read-only view of this project&apos;s workspace files.</p>
        </header>
        {root.kind === "preparing" && <SpinningUpStrip attempt={root.attempt} />}
        {root.kind === "retrying" && <RetryBanner attempt={root.attempt} onRetryNow={refreshRoot} />}
        <div className="file-explorer">
          <nav className="file-explorer__tree" aria-label="Workspace files" data-testid="files-tree">
            <ul>
              <SkeletonRows depth={0} />
            </ul>
          </nav>
          <div className="file-explorer__preview" data-testid="files-preview">
            <p className="muted">Select a file to preview its contents.</p>
          </div>
          <aside className="file-explorer__details" data-testid="files-details" aria-label="File details">
            <p className="muted">Select a file to see its details.</p>
          </aside>
        </div>
      </section>
    );
  }
  if (root.kind === "unauthenticated") {
    return honest("Not signed in", "Your session has expired — sign in to browse this project's files.");
  }
  if (root.kind === "not-found") {
    return honest("No files", "This project has no workspace, or you cannot access it.");
  }
  if (root.kind === "not-wired") {
    return honest(
      "File Explorer isn't available in this deployment yet",
      "The workspace reader isn't wired here yet — files browsing arrives with the next rollout.",
    );
  }
  if (root.kind === "error") {
    // ISI-5339 §4 terminal: retries exhausted / hard failure — an alert with a
    // manual [Try again]; the raw HTTP code stays in the tooltip, never as the
    // headline ("never a bare 504/500").
    return (
      <section data-testid="file-explorer">
        <h1>File Explorer</h1>
        <StateFrame
          tone="rose"
          badge="ERROR"
          icon="alert"
          title="We couldn't load these files."
          body="Something went wrong reading this workspace — it isn't your files that are missing. Try again in a moment."
          testId="files-error"
          alert
          action={
            <button type="button" className="btn" onClick={refreshRoot} data-testid="files-try-again">
              Try again
            </button>
          }
          detail={root.status ? `HTTP ${root.status}` : undefined}
        />
      </section>
    );
  }

  const rootEntries = root.data.entries;
  const degraded = root.data.degraded === true;
  // ISI-5339 §5 map — states bind to the ADR-0025 §D2 reason vocabulary
  // (ISI-5140 wire labels), never to bare HTTP codes or boolean guesses:
  //   degraded + workspace_busy + rows  → busy_snapshot (amber Snapshot pill)
  //   degraded + workspace_busy + none  → snapshot_unavailable (LIVE PAUSED)
  //   degraded=false + no_browse_target → no-run (NO RUN YET)
  //   plain 200 + 0 rows                → empty (EMPTY)
  const snapshotMode = degraded && root.data.reason === FILE_REASON_WORKSPACE_BUSY;
  const noTarget = !degraded && root.data.reason === FILE_REASON_NO_BROWSE_TARGET;

  // Frame 03·A: the pill is always present when a listing is shown.
  const pill = rootFetchedAt != null && rootEntries.length > 0 && (
    <FreshnessPill
      mode={snapshotMode ? "snapshot" : "live"}
      fetchedAt={rootFetchedAt}
      snapshotTakenAt={root.data.snapshotTakenAt}
    />
  );

  return (
    <section data-testid="file-explorer">
      <header className="file-explorer__header">
        <h1>File Explorer {pill}</h1>
        <p className="muted">Read-only view of this project&apos;s workspace files.</p>
      </header>

      {/* Frame 02 · busy_snapshot — a served snapshot with real rows: amber
          banner + Snapshot pill. "Real data, slightly stale", never an alarm. */}
      {snapshotMode && rootEntries.length > 0 && (
        <div className="banner banner--warn" role="status" data-testid="files-busy-banner">
          <strong>
            Showing a snapshot from{" "}
            {root.data.snapshotTakenAt ? new Date(root.data.snapshotTakenAt).toLocaleString() : "the last commit"}
          </strong>
          <p className="muted">
            An agent is editing this workspace right now, so you&apos;re seeing a point-in-time
            snapshot. The live view returns automatically when the agent is done.
          </p>
        </div>
      )}

      {/* Frame 02 · snapshot_unavailable — busy with NO servable snapshot. The
          single most important copy change in the spec (closes F1): a distinct
          labelled LIVE PAUSED state with a lock — NEVER an empty file panel. */}
      {snapshotMode && rootEntries.length === 0 ? (
        <StateFrame
          tone="slate"
          badge="LIVE PAUSED"
          icon="lock"
          title="Live view paused while the agent works"
          body="An agent is actively editing this workspace and a browsable snapshot isn't available for this workspace type yet. The file list returns when the agent releases it."
          testId="files-snapshot-unavailable"
        />
      ) : noTarget ? (
        /* Frame 02 · no-run — the honest "nothing has produced a workspace yet". */
        <StateFrame
          tone="slate"
          badge="NO RUN YET"
          icon="info"
          title="No files to show yet"
          body="This project hasn't produced a workspace yet. Files appear after the first run completes."
          testId="files-no-run"
        />
      ) : rootEntries.length === 0 ? (
        /* Frame 02 · empty — genuinely-empty workspace, distinct from no-run. */
        <StateFrame
          tone="slate"
          badge="EMPTY"
          icon="box"
          title="This workspace is empty"
          body="No files have been created here yet. Files appear here as agents work in this workspace."
          testId="files-empty"
        />
      ) : (
        <div className="file-explorer">
          <nav className="file-explorer__tree" aria-label="Workspace files" data-testid="files-tree">
            <ul>
              {sortEntries(rootEntries).map((e) => (
                <TreeNode
                  key={e.path}
                  projectId={projectId}
                  entry={e}
                  depth={0}
                  open={open}
                  dirs={dirs}
                  selected={selected}
                  onToggle={toggleDir}
                  onSelect={selectFile}
                />
              ))}
            </ul>
          </nav>
          <div className="file-explorer__preview" data-testid="files-preview">
            <PreviewPane projectId={projectId} path={selected} state={preview} />
          </div>
          <aside className="file-explorer__details" data-testid="files-details" aria-label="File details">
            <DetailsPane path={selected} state={stat} />
          </aside>
        </div>
      )}
    </section>
  );
}

/** One tree row. A directory toggles a lazy-loaded child listing; a file selects
 * itself into the preview. Directories render their loaded children recursively
 * once open. The only action affordance is the ISI-4652 download anchor beside
 * each row — a file downloads as an attachment, a directory as a tar.gz archive
 * (GET /files/download via the BFF). Still read-only: no edit/save/delete/
 * rename/run affordance, no context menu (§D3). */
function TreeNode({
  projectId,
  entry,
  depth,
  open,
  dirs,
  selected,
  onToggle,
  onSelect,
}: {
  projectId: string;
  entry: FileEntry;
  depth: number;
  open: Set<string>;
  dirs: Record<string, DirState>;
  selected: string | null;
  onToggle: (path: string) => void;
  onSelect: (path: string) => void;
}) {
  const isDir = entry.type === "dir";
  const isOpen = open.has(entry.path);
  const pad = { paddingLeft: 8 + depth * 14 };
  const fileGlyph = fileTypeGlyph(entry.name); // ISI-4747 follow-up: type-aware file icon
  // ISI-5339 Frame 01: while an open directory's listing is in flight, the row's
  // caret itself spins (inline per-row spinner) — the tree is never blocked and
  // every other row stays interactive.
  const childLoading =
    isDir && isOpen && (!dirs[entry.path] || dirs[entry.path].kind === "loading" || dirs[entry.path].kind === "preparing");

  // ISI-4652: per-row download. A sibling anchor (not nested in the row button —
  // interactive elements cannot nest), so a download click never toggles/selects.
  const download = (
    <a
      className="file-explorer__download"
      href={downloadProjectFileUrl(projectId, entry.path)}
      download
      aria-label={isDir ? `Download ${entry.name} as archive` : `Download ${entry.name}`}
      title={isDir ? "Download folder (.tar.gz)" : "Download file"}
      data-testid={isDir ? "files-download-dir" : "files-download-file"}
    >
      <TreeIcon name="download" />
    </a>
  );

  if (!isDir) {
    return (
      <li>
        <div className="file-explorer__rowwrap">
          <button
            type="button"
            className="file-explorer__row file-explorer__file"
            style={pad}
            aria-current={selected === entry.path ? "true" : undefined}
            data-testid="files-file"
            onClick={() => onSelect(entry.path)}
          >
            <span className="file-explorer__caret-spacer" aria-hidden="true" />
            <TreeIcon
              name={fileGlyph.icon}
              className={`file-explorer__glyph file-explorer__glyph--file file-explorer__glyph--${fileGlyph.tone}`}
            />
            <span className="file-explorer__label">{entry.name}</span>
          </button>
          {download}
        </div>
      </li>
    );
  }

  const childState = dirs[entry.path];
  return (
    <li>
      <div className="file-explorer__rowwrap">
        <button
          type="button"
          className="file-explorer__row file-explorer__dir"
          style={pad}
          aria-expanded={isOpen}
          data-testid="files-dir"
          onClick={() => onToggle(entry.path)}
        >
          <TreeIcon
            name="chevron"
            className={`file-explorer__caret${isOpen ? " file-explorer__caret--open" : ""}${childLoading ? " file-explorer__spin" : ""}`}
          />
          <TreeIcon
            name={isOpen ? "folderOpen" : "folder"}
            className="file-explorer__glyph file-explorer__glyph--dir"
          />
          <span className="file-explorer__label">{entry.name}</span>
        </button>
        {download}
      </div>
      {isOpen && (
        <ul>
          {!childState || childState.kind === "loading" || childState.kind === "preparing" ? (
            // ISI-5339 Frame 01: skeleton children indented beneath the row —
            // not a blocking full-tree spinner.
            <li data-testid="files-dir-loading" aria-busy="true">
              <ul>
                <SkeletonRows depth={depth + 1} count={3} />
              </ul>
            </li>
          ) : childState.kind === "ready" ? (
            childState.data.entries.length === 0 ? (
              <li className="muted" style={{ paddingLeft: 8 + (depth + 1) * 14 }}>
                (empty)
              </li>
            ) : depth + 1 > MAX_TREE_DEPTH ? (
              <li
                className="muted"
                style={{ paddingLeft: 8 + (depth + 1) * 14 }}
                data-testid="files-too-deep"
              >
                Too deeply nested to expand here — download the folder to see the rest.
              </li>
            ) : (
              sortEntries(childState.data.entries).map((c) => (
                <TreeNode
                  key={c.path}
                  projectId={projectId}
                  entry={c}
                  depth={depth + 1}
                  open={open}
                  dirs={dirs}
                  selected={selected}
                  onToggle={onToggle}
                  onSelect={onSelect}
                />
              ))
            )
          ) : (
            <li className="muted" style={{ paddingLeft: 8 + (depth + 1) * 14 }}>
              Couldn&apos;t load this folder.
            </li>
          )}
        </ul>
      )}
    </li>
  );
}

/** The right-hand details pane (ISI-4651, per the validated ISI-4602 mock): name,
 * path, type, size, modified, and the git last change (author / message / short
 * hash / commit time). Read-only — no action affordances (§D3). Stat failures are
 * honest notes, never fabricated metadata; the preview pane is unaffected. */
function DetailsPane({
  path,
  state,
}: {
  path: string | null;
  state: FilesState<FileStat>;
}) {
  if (!path) {
    return <p className="muted">Select a file to see its details.</p>;
  }
  if (state.kind === "loading") {
    return (
      <p className="muted" aria-busy="true" data-testid="files-details-loading">
        Loading details…
      </p>
    );
  }
  if (state.kind === "not-wired") {
    return <p className="muted">File details are not available in this deployment yet.</p>;
  }
  if (state.kind === "unauthenticated") {
    return <p className="muted">Your session has expired — sign in to see file details.</p>;
  }
  if (state.kind === "retrying") {
    return <p className="muted">This is taking longer than usual — retrying…</p>;
  }
  if (state.kind === "preparing") {
    return (
      <p className="muted" aria-busy="true" data-testid="files-details-preparing">
        Spinning up the file reader…
      </p>
    );
  }
  if (state.kind === "not-found" || state.kind === "error") {
    return (
      <p className="muted" data-testid="files-details-unavailable">
        Couldn&apos;t load details for <code>{path}</code>.
      </p>
    );
  }

  const s = state.data;
  return (
    <div>
      <h2 className="file-explorer__details-name" data-testid="files-details-name">
        {s.name}
      </h2>
      <dl className="file-explorer__details-list">
        <div>
          <dt>Path</dt>
          <dd>
            <code data-testid="files-details-path">{path}</code>
          </dd>
        </div>
        <div>
          <dt>Type</dt>
          <dd>{s.type === "dir" ? "Directory" : "File"}</dd>
        </div>
        <div>
          <dt>Size</dt>
          <dd data-testid="files-details-size">{humanBytes(s.size)}</dd>
        </div>
        <div>
          <dt>Modified</dt>
          <dd>{formatTimestamp(s.modTime)}</dd>
        </div>
      </dl>

      {s.degraded && (
        <div className="banner banner--info" role="status" data-testid="files-details-busy">
          Workspace busy — details reflect the last-committed snapshot.
        </div>
      )}

      <h3 className="file-explorer__details-change-title">Last change</h3>
      {s.git ? (
        <dl className="file-explorer__details-list" data-testid="files-details-change">
          <div>
            <dt>Author</dt>
            <dd data-testid="files-details-author">{s.git.author}</dd>
          </div>
          <div>
            <dt>Message</dt>
            <dd data-testid="files-details-message">{s.git.message}</dd>
          </div>
          <div>
            <dt>Commit</dt>
            <dd>
              <code data-testid="files-details-hash">{shortHash(s.git.commitHash)}</code>
            </dd>
          </div>
          <div>
            <dt>Committed</dt>
            <dd>{formatTimestamp(s.git.timestamp)}</dd>
          </div>
        </dl>
      ) : (
        <p className="muted" data-testid="files-details-nogit">
          No git history available for this file.
        </p>
      )}
    </div>
  );
}

/** The first 8 chars of a commit hash — the conventional short form. */
function shortHash(hash: string): string {
  return hash.length > 8 ? hash.slice(0, 8) : hash;
}

/** Render an RFC3339 timestamp for the details pane; falls back to the raw
 * string when it doesn't parse (never blanks the row). */
function formatTimestamp(ts: string): string {
  const d = new Date(ts);
  return Number.isNaN(d.getTime()) ? ts : d.toLocaleString();
}

/** The read-only preview pane. Renders loading (AC3), the binary placeholder
 * (AC2, never garbled text), a per-file degraded hint (AC4), and honest fetch
 * failures — never an edit surface (§D3). */
function PreviewPane({
  projectId,
  path,
  state,
}: {
  projectId: string;
  path: string | null;
  state: FilesState<FileContent>;
}) {
  if (!path) {
    return <p className="muted">Select a file to preview its contents.</p>;
  }
  if (state.kind === "loading") {
    return (
      <p className="muted" aria-busy="true" data-testid="files-preview-loading">
        Loading <code>{path}</code>…
      </p>
    );
  }
  if (state.kind === "not-found") {
    return <p className="muted" data-testid="files-preview-missing">That file is no longer available.</p>;
  }
  if (state.kind === "not-wired") {
    return <p className="muted">File preview is not available in this deployment yet.</p>;
  }
  if (state.kind === "unauthenticated") {
    return <p className="muted">Your session has expired — sign in to preview files.</p>;
  }
  if (state.kind === "retrying") {
    return <p className="muted">This is taking longer than usual — retrying…</p>;
  }
  if (state.kind === "preparing") {
    return (
      <p className="muted" aria-busy="true">
        Spinning up the file reader…
      </p>
    );
  }
  if (state.kind === "error") {
    return (
      <p className="muted" data-testid="files-preview-error">
        Couldn&apos;t load <code>{path}</code>
        {state.status ? ` (HTTP ${state.status})` : ""}.
      </p>
    );
  }

  return <FilePreviewBody projectId={projectId} c={state.data} />;
}

/** The ready-state preview body (ISI-4705). Split out of PreviewPane so the
 * expensive decode + syntax-highlight run inside `useMemo`: PreviewPane's parent
 * (`FileExplorerTabInner`) re-renders on every `open`/`dirs`/`selected`/`preview`/
 * `stat` change, and inlining these in the JSX re-decoded and re-highlighted the
 * whole (up to 1 MiB) window on each of those unrelated renders. */
function FilePreviewBody({ projectId, c }: { projectId: string; c: FileContent }) {
  const kind = previewKind(c.path, c.contentType);
  // The returned window size in bytes. Prefer the server's `length`, but that
  // field — exactly like `path` — is not on the Go struct and can be absent; fall
  // back to the base64 payload size (4 chars ≈ 3 bytes) so the large-file guard
  // never fails OPEN on a payload missing `length` (the same omit-a-field failure
  // mode this PR exists to fix).
  const windowBytes = c.length ?? Math.floor(((c.data?.length ?? 0) * 3) / 4);
  // Partial iff the window is smaller than the whole file. A missing `length`
  // reads as "unknown", never silently as "not truncated".
  const truncated = c.truncated === true || (c.length != null && c.length < c.size);
  // Guard EVERY text-ish renderer — code, markdown, AND plain <pre>: a 1 MiB
  // .log/.csv classifies as `text` and still bloats the DOM. Images/binary are
  // cheap placeholders and pass through.
  const richTooLarge =
    (kind === "code" || kind === "markdown" || kind === "text") &&
    windowBytes > RICH_PREVIEW_MAX_BYTES;

  // Decode once; skip entirely when the bytes are never rendered as text
  // (binary/image placeholders, or the too-large panel).
  const decoded = useMemo(
    () => (richTooLarge || kind === "binary" || kind === "image" ? "" : decodeTextContent(c.data)),
    [richTooLarge, kind, c.data],
  );
  // Highlight once per (bytes, path) — not on every parent re-render.
  const codeHtml = useMemo(
    () => (kind === "code" && !richTooLarge ? highlightCode(decoded, codeLanguage(c.path)) : ""),
    [kind, richTooLarge, decoded, c.path],
  );

  return (
    <div>
      <div className="file-explorer__preview-head">
        <code data-testid="files-preview-path">{c.path}</code>{" "}
        <span className="muted">· {humanBytes(c.size)}</span>
        {truncated && (
          <span className="muted" data-testid="files-preview-truncated"> · preview truncated (size cap)</span>
        )}
        {" · "}
        <a
          href={downloadProjectFileUrl(projectId, c.path)}
          download
          data-testid="files-preview-download"
        >
          Download
        </a>
      </div>

      {c.degraded && (
        <div className="banner banner--info" role="status" data-testid="files-preview-busy">
          Workspace busy — showing last-committed bytes.
        </div>
      )}

      {richTooLarge ? (
        <div data-testid="files-too-large">
          <p className="muted">
            This file is too large to preview here ({humanBytes(windowBytes)}
            {windowBytes < c.size ? ` of ${humanBytes(c.size)}` : ""}). Download it to view the full
            contents.
          </p>
          <a
            href={downloadProjectFileUrl(projectId, c.path)}
            download
            data-testid="files-too-large-download"
          >
            Download {fileName(c.path)}
          </a>
        </div>
      ) : kind === "binary" ? (
        <div data-testid="files-binary">
          <p className="muted">Binary file — {humanBytes(c.size)}. Not shown as text.</p>
          <a href={rawBytesDataUrl(c)} download={fileName(c.path)} data-testid="files-binary-download">
            Download raw bytes
          </a>
        </div>
      ) : kind === "image" ? (
        <div data-testid="files-image">
          <img className="file-explorer__image" src={imageDataUrl(c)} alt={fileName(c.path)} />
        </div>
      ) : kind === "markdown" ? (
        <div className="file-explorer__markdown" data-testid="files-markdown">
          <ReactMarkdown remarkPlugins={[remarkGfm]}>{decoded}</ReactMarkdown>
        </div>
      ) : kind === "code" ? (
        <pre className="file-explorer__code" data-testid="files-code">
          <code dangerouslySetInnerHTML={{ __html: codeHtml }} />
        </pre>
      ) : (
        <pre className="file-explorer__code" data-testid="files-text">
          {decoded}
        </pre>
      )}
    </div>
  );
}

/** Highlight a decoded source file with the grammar for its extension.
 * highlight.js escapes markup in its output, so the emitted HTML is safe to
 * inject. Falls back to HTML-escaped plain text if highlighting itself fails —
 * the preview never goes blank over a grammar edge case. */
function highlightCode(source: string, language: string | null): string {
  if (!language) return escapeHtml(source);
  try {
    return hljs.highlight(source, { language }).value;
  } catch {
    return escapeHtml(source);
  }
}

function escapeHtml(s: string): string {
  return s
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;");
}

function honest(title: string, why: string) {
  return (
    <section data-testid="file-explorer">
      <h1>File Explorer</h1>
      <EmptyState testId="files-honest" title={title} why={why} />
    </section>
  );
}

/** Directories first, then files, each alphabetical — the conventional tree order. */
function sortEntries(entries: FileEntry[]): FileEntry[] {
  return entries.slice().sort((a, b) => {
    if (a.type !== b.type) return a.type === "dir" ? -1 : 1;
    return a.name.localeCompare(b.name);
  });
}

function fileName(path: string): string {
  if (!path) return ""; // ISI-4705: never throw on a missing path (wire omits it)
  const i = path.lastIndexOf("/");
  return i >= 0 ? path.slice(i + 1) : path;
}
