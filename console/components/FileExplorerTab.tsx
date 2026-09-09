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
import "./file-explorer.css";
import { EmptyState } from "@/components/forms/EmptyState";
import {
  listProjectFiles,
  readProjectFile,
  decodeTextContent,
  humanBytes,
  rawBytesDataUrl,
  type FileEntry,
  type FileContent,
  type FilesState,
} from "@/lib/project-files";

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

      {c.contentType === "binary" ? (
        <div data-testid="files-binary">
          <p className="muted">Binary file — {humanBytes(c.size)}. Not shown as text.</p>
          <a href={rawBytesDataUrl(c)} download={fileName(c.path)} data-testid="files-binary-download">
            Download raw bytes
          </a>
        </div>
      ) : (
        <pre className="file-explorer__code" data-testid="files-text">
          {decodeTextContent(c.data)}
        </pre>
      )}
    </div>
  );
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
