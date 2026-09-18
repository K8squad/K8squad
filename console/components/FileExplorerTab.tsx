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

import { useCallback, useEffect, useState } from "react";
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
import {
  listProjectFiles,
  readProjectFile,
  statProjectFile,
  decodeTextContent,
  humanBytes,
  rawBytesDataUrl,
  previewKind,
  codeLanguage,
  imageDataUrl,
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
type DirState = FilesState<{ entries: FileEntry[]; degraded?: boolean }>;

export function FileExplorerTab({ projectId }: { projectId: string }) {
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

  const loadDir = useCallback(
    async (path: string): Promise<DirState> => {
      const state = await listProjectFiles(projectId, path);
      if (state.kind !== "ready") return state;
      return {
        kind: "ready",
        data: { entries: state.data.entries ?? [], degraded: state.data.degraded },
      };
    },
    [projectId],
  );

  // Load the root listing once (and on project change).
  useEffect(() => {
    let alive = true;
    setRoot({ kind: "loading" });
    setDirs({});
    setOpen(new Set());
    setSelected(null);
    loadDir("")
      .then((s) => {
        if (alive) setRoot(s);
      })
      .catch(() => {
        if (alive) setRoot({ kind: "error", status: 0 });
      });
    return () => {
      alive = false;
    };
  }, [loadDir]);

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

  // ——— terminal honest states for the whole tab (root listing) ———
  if (root.kind === "loading") {
    return (
      <section aria-busy="true" data-testid="files-loading">
        <h1>File Explorer</h1>
        <p className="muted">Loading workspace files…</p>
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
      "File Explorer not available yet",
      "The workspace reader is not wired in this deployment (the reader pod / files route is not exposed here yet).",
    );
  }
  if (root.kind === "error") {
    return honest(
      "Couldn't load files",
      `The workspace read failed${root.status ? ` (HTTP ${root.status})` : ""} — retry shortly.`,
    );
  }

  const rootEntries = root.data.entries;
  const degraded = root.data.degraded === true;

  return (
    <section data-testid="file-explorer">
      <header>
        <h1>File Explorer</h1>
        <p className="muted">Read-only view of this project&apos;s workspace files.</p>
      </header>

      {degraded && (
        <div className="banner banner--info" role="status" data-testid="files-busy-banner">
          Workspace busy — showing last-committed state. Files reflect the last commit, not
          live edits, while an agent holds the workspace.
        </div>
      )}

      {rootEntries.length === 0 ? (
        <EmptyState
          testId="files-empty"
          title="No files yet"
          why="This project's workspace has no files yet."
        />
      ) : (
        <div className="file-explorer">
          <nav className="file-explorer__tree" aria-label="Workspace files" data-testid="files-tree">
            <ul>
              {sortEntries(rootEntries).map((e) => (
                <TreeNode
                  key={e.path}
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
            <PreviewPane path={selected} state={preview} />
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
 * once open. There is NO context menu / action affordance — read-only (§D3). */
function TreeNode({
  entry,
  depth,
  open,
  dirs,
  selected,
  onToggle,
  onSelect,
}: {
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

  if (!isDir) {
    return (
      <li>
        <button
          type="button"
          className="file-explorer__row file-explorer__file"
          style={pad}
          aria-current={selected === entry.path ? "true" : undefined}
          data-testid="files-file"
          onClick={() => onSelect(entry.path)}
        >
          <span aria-hidden="true">📄</span> {entry.name}
        </button>
      </li>
    );
  }

  const childState = dirs[entry.path];
  return (
    <li>
      <button
        type="button"
        className="file-explorer__row file-explorer__dir"
        style={pad}
        aria-expanded={isOpen}
        data-testid="files-dir"
        onClick={() => onToggle(entry.path)}
      >
        <span aria-hidden="true">{isOpen ? "▾" : "▸"}</span> {entry.name}
      </button>
      {isOpen && (
        <ul>
          {!childState || childState.kind === "loading" ? (
            <li className="muted" style={{ paddingLeft: 8 + (depth + 1) * 14 }} data-testid="files-dir-loading">
              Loading…
            </li>
          ) : childState.kind === "ready" ? (
            childState.data.entries.length === 0 ? (
              <li className="muted" style={{ paddingLeft: 8 + (depth + 1) * 14 }}>
                (empty)
              </li>
            ) : (
              sortEntries(childState.data.entries).map((c) => (
                <TreeNode
                  key={c.path}
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
  path,
  state,
}: {
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
  if (state.kind === "error") {
    return (
      <p className="muted" data-testid="files-preview-error">
        Couldn&apos;t load <code>{path}</code>
        {state.status ? ` (HTTP ${state.status})` : ""}.
      </p>
    );
  }

  const c = state.data;
  const kind = previewKind(c.path, c.contentType);
  return (
    <div>
      <div className="file-explorer__preview-head">
        <code data-testid="files-preview-path">{c.path}</code>{" "}
        <span className="muted">· {humanBytes(c.size)}</span>
        {c.truncated && (
          <span className="muted" data-testid="files-preview-truncated"> · preview truncated (size cap)</span>
        )}
      </div>

      {c.degraded && (
        <div className="banner banner--info" role="status" data-testid="files-preview-busy">
          Workspace busy — showing last-committed bytes.
        </div>
      )}

      {kind === "binary" ? (
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
          <ReactMarkdown remarkPlugins={[remarkGfm]}>{decodeTextContent(c.data)}</ReactMarkdown>
        </div>
      ) : kind === "code" ? (
        <pre className="file-explorer__code" data-testid="files-code">
          <code
            dangerouslySetInnerHTML={{ __html: highlightCode(decodeTextContent(c.data), codeLanguage(c.path)) }}
          />
        </pre>
      ) : (
        <pre className="file-explorer__code" data-testid="files-text">
          {decodeTextContent(c.data)}
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
  const i = path.lastIndexOf("/");
  return i >= 0 ? path.slice(i + 1) : path;
}
