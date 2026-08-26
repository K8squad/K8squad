"use client";

// components/BuildBrowser.tsx — the story 8.7e three-pane build-browser screen body (ISI-2904).
//
// The build-workspace inspection view of ONE Run, fed by the git read-model through the BFF
// (app/api/runs/[runId]/build/[resource]). The apiserver applies the 8.7d per-principal +
// Team-scope gate and collapses every deny-or-missing path to 404 (existence-hiding) — so this
// component's 404 rendering is honest for BOTH "no such Run" and "not yours to see", with no way
// to distinguish them from the browser. It is the SAME 8.7d contract the 8.3 ArtifactBrowser
// renders, so the two sibling Run-scoped read models present a consistent existence-hiding story.
//
// Three panes:
//   • header  — the Run's build meta (head/base refs, changed-file count) from GET …/build/meta.
//   • left    — the file tree at the Run ref (GET …/build/tree?ref=run); selecting an entry loads
//               that file's bytes into the viewer.
//   • right   — the viewer, toggled between the unified Diff (GET …/build/diff) and a single File
//               (GET …/build/file?ref=run&path=…). Diff is the default view.
//
// Strictly read-only — inspection, not mutation. No POST/PUT/PATCH/DELETE is ever issued (the BFF
// route is GET-only and answers 405 to anything else); this matches the 8.7 read-model contract.

import { useEffect, useRef, useState } from "react";

/** One tree entry as the apiserver's git read-model lists it (buildbrowser.TreeEntry). */
export interface TreeEntry {
  path: string;
  mode: string;
  size: number;
}

/** GET …/build/tree — the file listing at a ref (buildbrowser.TreeResult). */
export interface TreeResult {
  ref: string;
  entries: TreeEntry[];
  truncated: boolean;
}

/** GET …/build/diff — the unified patch of the Run against its base (buildbrowser.DiffResult). */
export interface DiffResult {
  base: string;
  head: string;
  patch: string;
  truncated: boolean;
}

/** GET …/build/file — one file's bytes at a ref; content is base64 (Go []byte marshalling). */
export interface FileResult {
  ref: string;
  path: string;
  content: string;
  size: number;
  truncated: boolean;
}

/** GET …/build/meta — the Run's build coordinates (buildbrowser.MetaResult). */
export interface MetaResult {
  runId: string;
  head: string;
  base: string;
  changedFiles: number;
  /**
   * 8.7g PR/CI header-strip facts, populated by the Epic 11 SCM mirror when a Run's PR/CI is synced.
   * Both are optional: absent (omitempty on the server) when no PR/CI is synced, so the header strip
   * renders only when present and the browser degrades to git-only otherwise — no Epic 11 dependency.
   */
  prUrl?: string;
  ciStatus?: string;
}

type ScreenState =
  | { kind: "loading" }
  | { kind: "unauthenticated" }
  | { kind: "not-found" }
  | { kind: "not-wired" }
  | { kind: "error"; status: number }
  | { kind: "ready"; meta: MetaResult; tree: TreeResult; diff: DiffResult };

// The SAME status→state mapping the ArtifactBrowser uses, so the two Run-scoped read models keep
// one existence-hiding story: 404 is not-found (missing OR denied), 501 is the documented not-wired
// dev/DB-less answer, 401 is unauthenticated, everything else is a transient error.
export function classifyBuildStatus(status: number): Exclude<ScreenState, { kind: "ready" } | { kind: "loading" }> {
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

/** Decode the base64 file-content envelope to displayable text (best-effort for text blobs). */
export function decodeContent(b64: string): string {
  try {
    const bin = atob(b64);
    const bytes = Uint8Array.from(bin, (c) => c.charCodeAt(0));
    return new TextDecoder().decode(bytes);
  } catch {
    return "(undecodable content)";
  }
}

const preStyle: React.CSSProperties = {
  margin: 0,
  padding: 12,
  borderRadius: "var(--radius)",
  background: "color-mix(in srgb, var(--accent) 8%, transparent)",
  overflow: "auto",
  maxHeight: "60vh",
  fontFamily: "var(--font-mono)",
  fontSize: 13,
  whiteSpace: "pre",
};

type Viewer =
  | { kind: "diff" }
  | { kind: "file"; path: string; text: string; size: number; truncated: boolean };

export function BuildBrowser({ runId }: { runId: string }) {
  const [state, setState] = useState<ScreenState>({ kind: "loading" });
  const [viewer, setViewer] = useState<Viewer>({ kind: "diff" });
  const [loadingPath, setLoadingPath] = useState<string | null>(null);
  // Latest in-flight file request: a response is applied only if it is still the newest request,
  // so two rapid file clicks can't let the slower response overwrite the newer file's content
  // (the same staleness guard the ArtifactBrowser applies to its inspect path).
  const latestRequest = useRef<number>(0);

  useEffect(() => {
    let alive = true;
    const api = (resource: string) =>
      fetch(`/api/runs/${encodeURIComponent(runId)}/build/${resource}`, {
        headers: { accept: "application/json" },
      });
    // meta drives the not-wired / not-found / auth classification for the whole screen; tree and
    // diff are the two panes. All three share the 8.7d gate, so meta's status is authoritative for
    // the error states and a non-OK tree/diff after an OK meta is a transient read error.
    Promise.all([api("meta"), api("tree"), api("diff")])
      .then(async ([metaRes, treeRes, diffRes]) => {
        if (!alive) return;
        if (!metaRes.ok) {
          setState(classifyBuildStatus(metaRes.status));
          return;
        }
        if (!treeRes.ok || !diffRes.ok) {
          setState({ kind: "error", status: treeRes.ok ? diffRes.status : treeRes.status });
          return;
        }
        setState({
          kind: "ready",
          meta: (await metaRes.json()) as MetaResult,
          tree: (await treeRes.json()) as TreeResult,
          diff: (await diffRes.json()) as DiffResult,
        });
      })
      .catch(() => {
        if (alive) setState({ kind: "error", status: 0 });
      });
    return () => {
      alive = false;
    };
  }, [runId]);

  const openFile = async (path: string) => {
    const request = ++latestRequest.current;
    setLoadingPath(path);
    const stale = () => request !== latestRequest.current;
    try {
      const res = await fetch(
        `/api/runs/${encodeURIComponent(runId)}/build/file?ref=run&path=${encodeURIComponent(path)}`,
        { headers: { accept: "application/json" } },
      );
      if (stale()) return;
      if (!res.ok) {
        setViewer({ kind: "file", path, text: `(content unavailable — HTTP ${res.status})`, size: 0, truncated: false });
        return;
      }
      const body = (await res.json()) as FileResult;
      if (stale()) return;
      setViewer({ kind: "file", path, text: decodeContent(body.content), size: body.size, truncated: body.truncated });
    } catch {
      if (!stale()) setViewer({ kind: "file", path, text: "(content fetch failed)", size: 0, truncated: false });
    } finally {
      if (!stale()) setLoadingPath(null);
    }
  };

  if (state.kind === "loading") {
    return (
      <div className="card" data-testid="build-loading">
        Loading build workspace…
      </div>
    );
  }
  if (state.kind === "unauthenticated") {
    return (
      <div className="card" data-testid="build-unauthenticated">
        <p className="muted" style={{ margin: 0 }}>
          Sign in to inspect the build — the view is scoped to your session.
        </p>
      </div>
    );
  }
  // 404 renders the SAME card for a missing Run and a denied caller (existence-hiding).
  if (state.kind === "not-found") {
    return (
      <div className="card" data-testid="build-not-found">
        <p className="muted" style={{ margin: 0 }}>
          No build view for this Run — it does not exist or is not in your Team&apos;s scope.
        </p>
      </div>
    );
  }
  if (state.kind === "not-wired") {
    return (
      <div className="card" data-testid="build-not-wired">
        <p className="muted" style={{ margin: 0 }}>
          This apiserver runs without the build-browser read model (dev / DB-less run) and answers
          its documented 501.
        </p>
      </div>
    );
  }
  if (state.kind === "error") {
    return (
      <div className="card" data-testid="build-error">
        <p className="muted" style={{ margin: 0 }}>
          The build workspace could not be reached (HTTP {state.status || "network error"}). Retry
          shortly.
        </p>
      </div>
    );
  }

  const { meta, tree, diff } = state;

  return (
    <div data-testid="build-ready">
      <section className="card" data-testid="build-meta">
        <h2 style={{ margin: "0 0 4px" }}>Build workspace</h2>
        <p className="muted" style={{ margin: 0, fontSize: 13 }}>
          <code>{meta.head.slice(0, 12)}</code> vs base <code>{meta.base.slice(0, 12)}</code> ·{" "}
          {meta.changedFiles} changed {meta.changedFiles === 1 ? "file" : "files"}
        </p>
        <PrCiHeaderStrip prUrl={meta.prUrl} ciStatus={meta.ciStatus} />
      </section>

      <div
        style={{
          display: "grid",
          gridTemplateColumns: "minmax(220px, 320px) 1fr",
          gap: "var(--gap, 16px)",
          alignItems: "start",
        }}
      >
        <section className="card" data-testid="build-tree" style={{ overflow: "auto", maxHeight: "70vh" }}>
          <h3 style={{ margin: "0 0 8px" }}>Files</h3>
          {tree.entries.length === 0 ? (
            <p className="muted" style={{ margin: 0 }} data-testid="build-tree-empty">
              The workspace tree is empty at this ref.
            </p>
          ) : (
            <ul style={{ listStyle: "none", margin: 0, padding: 0 }}>
              {tree.entries.map((e) => (
                <li key={e.path}>
                  <button
                    type="button"
                    onClick={() => void openFile(e.path)}
                    disabled={loadingPath === e.path}
                    data-testid="build-tree-entry"
                    title={`${e.mode} · ${e.size} bytes`}
                    style={{
                      width: "100%",
                      textAlign: "left",
                      background:
                        viewer.kind === "file" && viewer.path === e.path
                          ? "color-mix(in srgb, var(--accent) 14%, transparent)"
                          : "transparent",
                      border: "none",
                      padding: "3px 6px",
                      borderRadius: "var(--radius)",
                      cursor: "pointer",
                      fontFamily: "var(--font-mono)",
                      fontSize: 13,
                    }}
                  >
                    {loadingPath === e.path ? "⋯ " : ""}
                    {e.path}
                  </button>
                </li>
              ))}
            </ul>
          )}
          {tree.truncated ? (
            <p className="muted" style={{ margin: "8px 0 0", fontSize: 12 }} data-testid="build-tree-truncated">
              Listing truncated at the server cap.
            </p>
          ) : null}
        </section>

        <section className="card" data-testid="build-viewer">
          <div style={{ display: "flex", gap: 8, marginBottom: 8 }}>
            <button
              type="button"
              onClick={() => setViewer({ kind: "diff" })}
              data-testid="build-tab-diff"
              aria-pressed={viewer.kind === "diff"}
              style={{ fontWeight: viewer.kind === "diff" ? 600 : 400 }}
            >
              Diff
            </button>
            {viewer.kind === "file" ? (
              <span className="muted" style={{ alignSelf: "center", fontSize: 12 }} data-testid="build-viewer-file-label">
                {viewer.path} · {viewer.size} bytes
                {viewer.truncated ? " · truncated at server cap" : ""}
              </span>
            ) : (
              <span className="muted" style={{ alignSelf: "center", fontSize: 12 }}>
                Unified diff of the Run against its base ref
                {diff.truncated ? " · truncated at server cap" : ""}
              </span>
            )}
          </div>
          {viewer.kind === "diff" ? (
            diff.patch.length === 0 ? (
              <p className="muted" style={{ margin: 0 }} data-testid="build-diff-empty">
                No changes against the base ref.
              </p>
            ) : (
              <pre style={preStyle} data-testid="build-diff">
                {diff.patch}
              </pre>
            )
          ) : (
            <pre style={preStyle} data-testid="build-file">
              {viewer.text}
            </pre>
          )}
        </section>
      </div>
    </div>
  );
}

/**
 * PrCiHeaderStrip is the 8.7g PR/CI header strip. It renders the Run's pull-request link and CI
 * status ONLY when the Epic 11 SCM mirror has synced them (prUrl/ciStatus present). With neither
 * present it renders nothing — the build browser degrades to git-only and never depends on Epic 11
 * to ship (8.7g AC). A ciStatus with no prUrl (or vice-versa) still renders the part that is present.
 */
function PrCiHeaderStrip({ prUrl, ciStatus }: { prUrl?: string; ciStatus?: string }) {
  if (!prUrl && !ciStatus) return null;
  return (
    <div
      data-testid="build-pr-ci-strip"
      style={{ marginTop: 6, display: "flex", gap: 10, alignItems: "center", fontSize: 13 }}
    >
      {prUrl ? (
        <a data-testid="build-pr-link" href={prUrl} target="_blank" rel="noopener noreferrer">
          View pull request
        </a>
      ) : null}
      {ciStatus ? (
        <span data-testid="build-ci-status" className="muted">
          CI: {ciStatus}
        </span>
      ) : null}
    </div>
  );
}
