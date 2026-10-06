// test/projects/FileExplorerTab.test.tsx — ISI-3956 S4c: the File Explorer tab.
// Covers AC1 tree lazy-expand + read-only preview (no mutate affordance),
// AC2 binary placeholder (no garbled text), AC3 loading, AC4 workspace-busy
// degraded banner over the snapshot, AC5 empty + 501, and 404 existence-hiding.

import { describe, it, expect, afterEach, vi } from "vitest";
import { render, screen, cleanup, waitFor, fireEvent, within, act } from "@testing-library/react";
import { FileExplorerTab, FileExplorerErrorBoundary } from "@/components/FileExplorerTab";
import { normalizeListing, normalizeContent, cacheListing, __resetListingCache } from "@/lib/project-files";
import { reportClientError } from "@/lib/client-errors";
import type {
  FileContent,
  FileListing,
  FileStat,
  WireFileListing,
  WireFileContent,
} from "@/lib/project-files";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  vi.useRealTimers();
  // ISI-5485: the D5a listing cache is module-scoped — reset it between cases so
  // a seeded entry never leaks across tests.
  __resetListingCache();
});

/** base64 of a UTF-8 string (jsdom has Buffer). */
const b64 = (s: string) => Buffer.from(s, "utf-8").toString("base64");

/** A 503 retryable-degraded Response (ADR-0025 D2 taxonomy body), with the
 * clone() seam isRetryableDegraded reads. */
const retryable503 = (): Response => {
  const body = { error: "workspace reader timed out; retry shortly", code: "retryable_degraded" };
  return {
    ok: false,
    status: 503,
    json: () => Promise.resolve(body),
    clone: () => ({ json: () => Promise.resolve(body) }),
  } as unknown as Response;
};

/** Route the BFF endpoints to per-path canned responses. `listings` is keyed
 * by the `path` query (root = ""); `contents` and `stats` by the file path.
 * A key that maps to a number is returned as that HTTP status with a null body. */
function routeFetch(opts: {
  // Listings are typed as the WIRE shape (path optional) — that is what S4b
  // actually sends, and it lets a fixture omit `path` exactly like production
  // (ISI-4705). A full FileListing is still assignable here.
  listings?: Record<string, WireFileListing | number>;
  contents?: Record<string, WireFileContent | number>;
  stats?: Record<string, FileStat | number>;
  status?: number;
}) {
  const spy = vi.fn((url: string) => {
    const u = new URL(url, "http://localhost");
    const isContent = u.pathname.endsWith("/files/content");
    const isStat = u.pathname.endsWith("/files/stat");
    const path = u.searchParams.get("path") ?? "";
    let entry: unknown = opts.status;
    if (isContent) entry = opts.contents?.[path];
    else if (isStat) entry = opts.stats?.[path];
    else entry = opts.listings?.[path];
    if (entry === undefined) entry = 404;
    const status = typeof entry === "number" ? entry : 200;
    const body = typeof entry === "number" ? null : entry;
    return Promise.resolve({
      ok: status >= 200 && status < 300,
      status,
      json: () => Promise.resolve(body),
      text: () => Promise.resolve(JSON.stringify(body)),
    } as Response);
  });
  vi.stubGlobal("fetch", spy as unknown as typeof fetch);
  return spy;
}

describe("FileExplorerTab", () => {
  it("renders the tree, lazily expands a directory, and previews a text file read-only (AC1)", async () => {
    routeFetch({
      listings: {
        "": { path: "", entries: [{ name: "src", path: "src", type: "dir" }, { name: "README.md", path: "README.md", type: "file", size: 12 }] },
        src: { path: "src", entries: [{ name: "main.ts", path: "src/main.ts", type: "file", size: 20 }] },
      },
      contents: {
        "src/main.ts": { path: "src/main.ts", size: 20, contentType: "text", data: b64("export const x=1"), offset: 0, length: 20 },
      },
    });
    render(<FileExplorerTab projectId="web" />);

    await waitFor(() => expect(screen.getByTestId("file-explorer")).toBeTruthy());
    // Root shows the dir + file; the nested file is NOT in the DOM until expand (lazy).
    expect(screen.getByText("src")).toBeTruthy();
    expect(screen.queryByText("main.ts")).toBeNull();

    fireEvent.click(screen.getByText("src"));
    await waitFor(() => expect(screen.getByText("main.ts")).toBeTruthy());

    fireEvent.click(screen.getByText("main.ts"));
    // ISI-4648: a .ts file previews as highlighted code — the source text is
    // preserved verbatim inside the token spans.
    await waitFor(() => expect(screen.getByTestId("files-code")).toBeTruthy());
    expect(screen.getByTestId("files-code").textContent).toBe("export const x=1");

    // Read-only (§D3): no edit/save/delete/rename/run affordance anywhere.
    const root = screen.getByTestId("file-explorer");
    for (const forbidden of [/save/i, /delete/i, /rename/i, /\brun\b/i, /edit/i]) {
      expect(within(root).queryByText(forbidden)).toBeNull();
    }
  });

  it("renders a markdown file as formatted content, not raw source (ISI-4648)", async () => {
    routeFetch({
      listings: { "": { path: "", entries: [{ name: "README.md", path: "README.md", type: "file", size: 30 }] } },
      contents: {
        "README.md": { path: "README.md", size: 30, contentType: "text", data: b64("# Hello\n\nsome **bold** text"), offset: 0, length: 30 },
      },
    });
    render(<FileExplorerTab projectId="web" />);
    await waitFor(() => expect(screen.getByTestId("file-explorer")).toBeTruthy());
    fireEvent.click(screen.getByText("README.md"));
    await waitFor(() => expect(screen.getByTestId("files-markdown")).toBeTruthy());
    const md = screen.getByTestId("files-markdown");
    // Rendered: an <h1> and a <strong>, not the raw "# Hello" / "**bold**" source.
    expect(within(md).getByRole("heading", { level: 1 }).textContent).toBe("Hello");
    expect(md.querySelector("strong")?.textContent).toBe("bold");
    expect(screen.queryByTestId("files-text")).toBeNull();
  });

  it("renders a png as an image even when the wire hint is binary (ISI-4648)", async () => {
    routeFetch({
      listings: { "": { path: "", entries: [{ name: "logo.png", path: "logo.png", type: "file", size: 2048 }] } },
      contents: { "logo.png": { path: "logo.png", size: 2048, contentType: "binary", data: b64("\x89PNG\r\n"), offset: 0, length: 2048 } },
    });
    render(<FileExplorerTab projectId="web" />);
    await waitFor(() => expect(screen.getByTestId("file-explorer")).toBeTruthy());
    fireEvent.click(screen.getByText("logo.png"));
    await waitFor(() => expect(screen.getByTestId("files-image")).toBeTruthy());
    const img = screen.getByTestId("files-image").querySelector("img");
    expect(img?.getAttribute("src")).toMatch(/^data:image\/png;base64,/);
    // An image is never the binary placeholder nor garbled text.
    expect(screen.queryByTestId("files-binary")).toBeNull();
    expect(screen.queryByTestId("files-text")).toBeNull();
  });

  it("renders an svg as an image with the svg mime (ISI-4648)", async () => {
    routeFetch({
      listings: { "": { path: "", entries: [{ name: "icon.svg", path: "icon.svg", type: "file", size: 40 }] } },
      contents: { "icon.svg": { path: "icon.svg", size: 40, contentType: "text", data: b64("<svg xmlns='x'></svg>"), offset: 0, length: 40 } },
    });
    render(<FileExplorerTab projectId="web" />);
    await waitFor(() => expect(screen.getByTestId("file-explorer")).toBeTruthy());
    fireEvent.click(screen.getByText("icon.svg"));
    await waitFor(() => expect(screen.getByTestId("files-image")).toBeTruthy());
    const img = screen.getByTestId("files-image").querySelector("img");
    expect(img?.getAttribute("src")).toMatch(/^data:image\/svg\+xml;base64,/);
  });

  it("renders a go file with syntax-highlight token spans (ISI-4648)", async () => {
    routeFetch({
      listings: { "": { path: "", entries: [{ name: "main.go", path: "main.go", type: "file", size: 30 }] } },
      contents: { "main.go": { path: "main.go", size: 30, contentType: "text", data: b64('package main\n// hi\nvar s = "x"'), offset: 0, length: 30 } },
    });
    render(<FileExplorerTab projectId="web" />);
    await waitFor(() => expect(screen.getByTestId("file-explorer")).toBeTruthy());
    fireEvent.click(screen.getByText("main.go"));
    await waitFor(() => expect(screen.getByTestId("files-code")).toBeTruthy());
    const code = screen.getByTestId("files-code");
    expect(code.querySelector(".hljs-keyword")).toBeTruthy();
    expect(code.querySelector(".hljs-string")).toBeTruthy();
    expect(code.textContent).toBe('package main\n// hi\nvar s = "x"');
  });

  it("still shows plain text for an unknown text extension (ISI-4648)", async () => {
    routeFetch({
      listings: { "": { path: "", entries: [{ name: "notes.txt", path: "notes.txt", type: "file", size: 5 }] } },
      contents: { "notes.txt": { path: "notes.txt", size: 5, contentType: "text", data: b64("hello"), offset: 0, length: 5 } },
    });
    render(<FileExplorerTab projectId="web" />);
    await waitFor(() => expect(screen.getByTestId("file-explorer")).toBeTruthy());
    fireEvent.click(screen.getByText("notes.txt"));
    await waitFor(() => expect(screen.getByTestId("files-text")).toBeTruthy());
    expect(screen.getByTestId("files-text").textContent).toBe("hello");
  });

  it("shows a binary placeholder with byte count, never garbled text (AC2)", async () => {
    routeFetch({
      listings: { "": { path: "", entries: [{ name: "blob.so", path: "blob.so", type: "file", size: 2048 }] } },
      contents: { "blob.so": { path: "blob.so", size: 2048, contentType: "binary", data: b64("\x7fELF\x00"), offset: 0, length: 2048 } },
    });
    render(<FileExplorerTab projectId="web" />);
    await waitFor(() => expect(screen.getByTestId("file-explorer")).toBeTruthy());
    fireEvent.click(screen.getByText("blob.so"));
    await waitFor(() => expect(screen.getByTestId("files-binary")).toBeTruthy());
    const binary = screen.getByTestId("files-binary");
    expect(within(binary).getByText(/Binary file/)).toBeTruthy();
    expect(within(binary).getByText(/2\.0 KB/)).toBeTruthy();
    // Never rendered as a text <pre>.
    expect(screen.queryByTestId("files-text")).toBeNull();
    // Read-only download of raw bytes is a data: URL (no write path into the volume).
    expect(screen.getByTestId("files-binary-download").getAttribute("href")).toMatch(/^data:application\/octet-stream;base64,/);
  });

  it("shows the details pane with size, modified, and git last change (ISI-4651)", async () => {
    routeFetch({
      listings: { "": { path: "", entries: [{ name: "main.go", path: "main.go", type: "file", size: 30 }] } },
      contents: {
        "main.go": { path: "main.go", size: 30, contentType: "text", data: b64("package main"), offset: 0, length: 30 },
      },
      stats: {
        "main.go": {
          name: "main.go",
          type: "file",
          size: 30,
          modTime: "2026-09-17T10:00:00Z",
          git: {
            commitHash: "abcdef1234567890",
            author: "Agent Amelia",
            message: "wire the reader pod",
            timestamp: "2026-09-17T09:00:00Z",
          },
        },
      },
    });
    render(<FileExplorerTab projectId="web" />);
    await waitFor(() => expect(screen.getByTestId("file-explorer")).toBeTruthy());

    // No selection yet — the pane prompts instead of fabricating metadata.
    expect(screen.getByText(/Select a file to see its details/)).toBeTruthy();

    fireEvent.click(screen.getByText("main.go"));
    await waitFor(() => expect(screen.getByTestId("files-details-change")).toBeTruthy());
    const details = screen.getByTestId("files-details");
    expect(within(details).getByTestId("files-details-name").textContent).toBe("main.go");
    expect(within(details).getByTestId("files-details-path").textContent).toBe("main.go");
    expect(within(details).getByTestId("files-details-size").textContent).toBe("30 B");
    expect(within(details).getByText("Modified")).toBeTruthy();
    expect(within(details).getByTestId("files-details-author").textContent).toBe("Agent Amelia");
    expect(within(details).getByTestId("files-details-message").textContent).toBe("wire the reader pod");
    expect(within(details).getByTestId("files-details-hash").textContent).toBe("abcdef12");
    // Read-only (§D3): the details pane carries no action affordances.
    for (const forbidden of [/save/i, /delete/i, /rename/i, /edit/i]) {
      expect(within(details).queryByText(forbidden)).toBeNull();
    }
  });

  it("shows an honest no-git note when the workspace has no git history (ISI-4651)", async () => {
    routeFetch({
      listings: { "": { path: "", entries: [{ name: "notes.txt", path: "notes.txt", type: "file", size: 5 }] } },
      contents: {
        "notes.txt": { path: "notes.txt", size: 5, contentType: "text", data: b64("hello"), offset: 0, length: 5 },
      },
      stats: {
        "notes.txt": { name: "notes.txt", type: "file", size: 5, modTime: "2026-09-17T10:00:00Z" },
      },
    });
    render(<FileExplorerTab projectId="web" />);
    await waitFor(() => expect(screen.getByTestId("file-explorer")).toBeTruthy());
    fireEvent.click(screen.getByText("notes.txt"));
    await waitFor(() => expect(screen.getByTestId("files-details-nogit")).toBeTruthy());
    expect(screen.getByText(/No git history available/)).toBeTruthy();
    // The base metadata still renders — no-git is a fallback, not an error.
    expect(screen.getByTestId("files-details-size").textContent).toBe("5 B");
  });

  it("keeps the preview working when the stat fetch fails (ISI-4651)", async () => {
    routeFetch({
      listings: { "": { path: "", entries: [{ name: "notes.txt", path: "notes.txt", type: "file", size: 5 }] } },
      contents: {
        "notes.txt": { path: "notes.txt", size: 5, contentType: "text", data: b64("hello"), offset: 0, length: 5 },
      },
      stats: { "notes.txt": 500 },
    });
    render(<FileExplorerTab projectId="web" />);
    await waitFor(() => expect(screen.getByTestId("file-explorer")).toBeTruthy());
    fireEvent.click(screen.getByText("notes.txt"));
    await waitFor(() => expect(screen.getByTestId("files-details-unavailable")).toBeTruthy());
    // Preview is unaffected by the stat failure.
    await waitFor(() => expect(screen.getByTestId("files-text")).toBeTruthy());
    expect(screen.getByTestId("files-text").textContent).toBe("hello");
  });

  it("offers a per-file download on the row and in the preview head (ISI-4652)", async () => {
    routeFetch({
      listings: { "": { path: "", entries: [{ name: "README.md", path: "README.md", type: "file", size: 30 }] } },
      contents: {
        "README.md": { path: "README.md", size: 30, contentType: "text", data: b64("# Hello"), offset: 0, length: 30 },
      },
    });
    render(<FileExplorerTab projectId="web" />);
    await waitFor(() => expect(screen.getByTestId("file-explorer")).toBeTruthy());

    // Per-row file download: a plain <a download> against the BFF download route.
    const rowDownload = screen.getByTestId("files-download-file");
    expect(rowDownload.getAttribute("href")).toBe("/api/projects/web/files/download?path=README.md");
    expect(rowDownload.getAttribute("download")).not.toBeNull();
    expect(rowDownload.getAttribute("aria-label")).toBe("Download README.md");

    // The preview head carries the same server-side download for the selected file.
    fireEvent.click(screen.getByText("README.md"));
    await waitFor(() => expect(screen.getByTestId("files-markdown")).toBeTruthy());
    expect(screen.getByTestId("files-preview-download").getAttribute("href")).toBe(
      "/api/projects/web/files/download?path=README.md",
    );
  });

  it("offers a per-folder archive download with an encoded path (ISI-4652)", async () => {
    routeFetch({
      listings: {
        "": { path: "", entries: [{ name: "my dir", path: "my dir", type: "dir" }] },
        "my dir": { path: "my dir", entries: [{ name: "a file.txt", path: "my dir/a file.txt", type: "file", size: 5 }] },
      },
    });
    render(<FileExplorerTab projectId="web" />);
    await waitFor(() => expect(screen.getByTestId("file-explorer")).toBeTruthy());

    // Folder archive download on the dir row, paths URL-encoded.
    const dirDownload = screen.getByTestId("files-download-dir");
    expect(dirDownload.getAttribute("href")).toBe("/api/projects/web/files/download?path=my%20dir");
    expect(dirDownload.getAttribute("aria-label")).toBe("Download my dir as archive");

    // Nested rows get the same affordance after lazy expand.
    fireEvent.click(screen.getByText("my dir"));
    await waitFor(() => expect(screen.getByText("a file.txt")).toBeTruthy());
    expect(screen.getByTestId("files-download-file").getAttribute("href")).toBe(
      "/api/projects/web/files/download?path=my%20dir%2Fa%20file.txt",
    );
  });

  it("never downloads through the JSON fetchers — the download anchor is a plain href (ISI-4652)", async () => {
    const spy = routeFetch({
      listings: { "": { path: "", entries: [{ name: "notes.txt", path: "notes.txt", type: "file", size: 5 }] } },
    });
    render(<FileExplorerTab projectId="web" />);
    await waitFor(() => expect(screen.getByTestId("file-explorer")).toBeTruthy());
    // The download route is only ever referenced as an href, never fetched as JSON.
    expect(spy.mock.calls.some(([url]) => String(url).includes("/files/download"))).toBe(false);
  });

  it("renders a loading state while the root tree is fetching (AC3)", async () => {
    // A fetch that never resolves keeps the tab in its loading state.
    vi.stubGlobal("fetch", vi.fn(() => new Promise(() => {})) as unknown as typeof fetch);
    render(<FileExplorerTab projectId="web" />);
    expect(screen.getByTestId("files-loading")).toBeTruthy();
  });

  // ISI-5339 Frame 01: loading is PROGRESSIVE — the shell paints instantly with
  // skeleton rows, never a full-tab blank spinner.
  it("paints the tab shell with skeleton rows instead of a blank loading panel (ISI-5339 F01)", async () => {
    vi.stubGlobal("fetch", vi.fn(() => new Promise(() => {})) as unknown as typeof fetch);
    render(<FileExplorerTab projectId="web" />);
    const tab = screen.getByTestId("files-loading");
    expect(screen.getAllByText("File Explorer").length).toBeGreaterThan(0);
    // Skeleton bars render, are aria-hidden, and the region is aria-busy.
    const skel = screen.getAllByTestId("files-skeleton");
    expect(skel.length).toBeGreaterThanOrEqual(3);
    expect(screen.getByRole("region", { name: "Loading files…" }) as HTMLElement).toBeTruthy();
    expect(tab.getAttribute("aria-busy")).toBe("true");
  });

  // ISI-5339 Frame 01 + D2 taxonomy: a 202 preparing response must NEVER be
  // parsed as an ok listing (202 is 2xx!) — that fabricated-empty bug is exactly
  // what the honest-states spec exists to kill.
  it("shows the bounded spinning-up strip on 202 preparing, never a fabricated empty tree (ISI-5339 F01)", async () => {
    let call = 0;
    vi.stubGlobal(
      "fetch",
      vi.fn(() => {
        call++;
        if (call === 1) {
          // 202 preparing: ok=true on the wire — the trap.
          return Promise.resolve({
            ok: true,
            status: 202,
            json: () => Promise.resolve({ error: "reader pod is warming up; retry shortly", code: "preparing" }),
          } as unknown as Response);
        }
        return Promise.resolve({
          ok: true,
          status: 200,
          json: () =>
            Promise.resolve({
              path: "",
              entries: [{ name: "README.md", path: "README.md", type: "file", size: 12 }],
            }),
        } as unknown as Response);
      }) as unknown as typeof fetch,
    );
    render(<FileExplorerTab projectId="web" />);

    await waitFor(() => expect(screen.getByTestId("files-preparing")).toBeTruthy());
    expect(screen.getByText("Spinning up the file reader…")).toBeTruthy();
    expect(screen.getByText(/This usually takes a few seconds/)).toBeTruthy();
    // The determinate-feeling progress bar advances with the bounded budget.
    expect(screen.getByTestId("files-preparing").querySelector(".file-explorer__progress-fill")).toBeTruthy();
    // NOT an empty listing, NOT an empty state — the strip holds the shell.
    expect(screen.queryByTestId("files-empty")).toBeNull();
    expect(screen.queryByTestId("files-no-run")).toBeNull();
    expect(screen.getAllByTestId("files-skeleton").length).toBeGreaterThan(0);

    // The poll lands: the strip is replaced by the real tree.
    await waitFor(
      () => expect(screen.getByTestId("file-explorer")).toBeTruthy(),
      { timeout: 4000 },
    );
    expect(screen.getByText("README.md")).toBeTruthy();
    expect(screen.queryByTestId("files-preparing")).toBeNull();
  });

  it("shows the workspace-busy degraded banner over the last-committed snapshot (AC4)", async () => {
    routeFetch({
      // Mirror the server contract (files.go): a workspace-busy snapshot answers
      // degraded=true WITH reason="workspace_busy" — the banner keys on the reason,
      // not degraded alone (ISI-5140).
      listings: { "": { path: "", degraded: true, reason: "workspace_busy", entries: [{ name: "app.go", path: "app.go", type: "file", size: 8 }] } },
    });
    render(<FileExplorerTab projectId="web" />);
    await waitFor(() => expect(screen.getByTestId("files-busy-banner")).toBeTruthy());
    // ISI-5339 F02 busy_snapshot copy: human, not engine-speak.
    expect(screen.getByText(/Showing a snapshot from/)).toBeTruthy();
    expect(screen.getByText(/point-in-time snapshot/)).toBeTruthy();
    // The snapshot rows still render — busy is informational, never a blank error.
    expect(screen.getByText("app.go")).toBeTruthy();
    // Frame 03: the amber Snapshot pill is present with the banner.
    expect(screen.getByTestId("files-pill-snapshot").textContent).toMatch(/Snapshot · /);
  });

  // ISI-5339 F02 snapshot_unavailable — the F1 closer: busy with NO servable
  // snapshot is a distinct LIVE PAUSED state, NEVER an empty file panel.
  it("renders LIVE PAUSED (lock) for busy-with-no-snapshot — never an empty listing (ISI-5339 F02)", async () => {
    routeFetch({
      listings: { "": { path: "", degraded: true, reason: "workspace_busy", entries: [] } },
    });
    render(<FileExplorerTab projectId="web" />);
    await waitFor(() => expect(screen.getByTestId("files-snapshot-unavailable")).toBeTruthy());
    expect(screen.getByText("Live view paused while the agent works")).toBeTruthy();
    expect(screen.getByText(/returns when the agent releases it/)).toBeTruthy();
    expect(screen.getByText("LIVE PAUSED")).toBeTruthy();
    // The critical distinction: it is NOT the empty state and NOT the snapshot banner.
    expect(screen.queryByTestId("files-empty")).toBeNull();
    expect(screen.queryByTestId("files-no-run")).toBeNull();
    expect(screen.queryByTestId("files-busy-banner")).toBeNull();
  });

  it("renders 'This workspace is empty' for an empty workspace (AC5)", async () => {
    routeFetch({ listings: { "": { path: "", entries: [] } } });
    render(<FileExplorerTab projectId="web" />);
    await waitFor(() => expect(screen.getByTestId("files-empty")).toBeTruthy());
    expect(screen.getByText("This workspace is empty")).toBeTruthy();
    expect(screen.getByText(/No files have been created here yet/)).toBeTruthy();
  });

  // ISI-5339 F02 no-run: distinct from genuinely-empty (different badge + copy).
  it("renders the NO RUN YET state distinct from empty (ISI-5339 F02)", async () => {
    routeFetch({
      listings: { "": { path: "", entries: [], reason: "no_browse_target" } },
    });
    render(<FileExplorerTab projectId="web" />);
    await waitFor(() => expect(screen.getByTestId("files-no-run")).toBeTruthy());
    expect(screen.getByText("No files to show yet")).toBeTruthy();
    expect(screen.getByText(/hasn't produced a workspace yet/)).toBeTruthy();
    expect(screen.getByText("NO RUN YET")).toBeTruthy();
    expect(screen.queryByTestId("files-empty")).toBeNull();
  });

  // ISI-5339 F03·A: the Live pill with green dot + relative freshness.
  it("shows the Live freshness pill on a live listing (ISI-5339 F03)", async () => {
    routeFetch({
      listings: { "": { path: "", entries: [{ name: "README.md", path: "README.md", type: "file", size: 12 }] } },
    });
    render(<FileExplorerTab projectId="web" />);
    await waitFor(() => expect(screen.getByTestId("files-pill-live")).toBeTruthy());
    expect(screen.getByTestId("files-pill-live").textContent).toMatch(/Live · just now/);
    expect(screen.getByTestId("files-pill-live").querySelector(".file-explorer__pill-dot")).toBeTruthy();
    // Tooltip carries the accessible absolute timestamp.
    expect(screen.getByTestId("files-pill-live").getAttribute("title")).toMatch(/Live workspace — refreshed /);
  });

  // ISI-5339 F01: lazy expand is an inline per-row spinner + skeleton children;
  // the tree is never blocked by a pending directory fetch.
  it("expands lazily with an inline row spinner and skeleton children — the tree never blocks (ISI-5339 F01)", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn((url: string) => {
        const u = new URL(url, "http://localhost");
        if (u.searchParams.get("path") === "src") return new Promise(() => {}); // pending child listing
        return Promise.resolve({
          ok: true,
          status: 200,
          json: () =>
            Promise.resolve({
              path: "",
              entries: [
                { name: "src", path: "src", type: "dir" },
                { name: "README.md", path: "README.md", type: "file", size: 5 },
              ],
            }),
        } as unknown as Response);
      }) as unknown as typeof fetch,
    );
    render(<FileExplorerTab projectId="web" />);
    await waitFor(() => expect(screen.getByTestId("file-explorer")).toBeTruthy());

    fireEvent.click(screen.getByText("src"));
    // Inline per-row spinner: the src row's caret spins.
    const srcBtn = screen.getByText("src").closest("button");
    expect(srcBtn?.querySelector(".file-explorer__spin")).toBeTruthy();
    // Skeleton children indented beneath the row.
    expect(screen.getByTestId("files-dir-loading")).toBeTruthy();
    // The rest of the tree stays interactive — no full-tab block.
    expect(screen.getByText("README.md")).toBeTruthy();
    expect(screen.getByTestId("files-preview")).toBeTruthy();
  });

  it("renders honest 'not available yet' on 501 (AC5)", async () => {
    routeFetch({ listings: { "": 501 } });
    render(<FileExplorerTab projectId="web" />);
    await waitFor(() => expect(screen.getByTestId("files-honest")).toBeTruthy());
    expect(screen.getByText(/isn't available in this deployment yet/)).toBeTruthy();
  });

  it("renders an existence-hiding not-found on 404 (no leak)", async () => {
    routeFetch({ listings: { "": 404 } });
    render(<FileExplorerTab projectId="web" />);
    await waitFor(() => expect(screen.getByTestId("files-honest")).toBeTruthy());
    expect(screen.getByText(/No files/)).toBeTruthy();
    // Never leaks a distinguishing "forbidden" vs "missing" — 404 is uniform.
    expect(screen.queryByText(/forbidden|403|permission/i)).toBeNull();
  });

  // ——— ISI-5339 (S6): Frame 03·B retry affordance ———

  it("shows the amber retry banner with attempt budget and [Retry now] on 503 read-timeout (ISI-5339 F03)", async () => {
    const spy = vi.fn(() => Promise.resolve(retryable503()));
    vi.stubGlobal("fetch", spy as unknown as typeof fetch);
    render(<FileExplorerTab projectId="web" />);

    await waitFor(() => expect(screen.getByTestId("files-retrying")).toBeTruthy());
    expect(screen.getByText(/This is taking longer than usual — retrying/)).toBeTruthy();
    expect(screen.getByText(/attempt 1 of 3/i)).toBeTruthy();
    expect(screen.getByText(/your place in the tree is kept/i)).toBeTruthy();
    // Human copy, never a bare 503.
    expect(screen.queryByText(/503/)).toBeNull();
    expect(screen.getByTestId("files-retry-now")).toBeTruthy();
    // Progressive shell: skeleton rows under the banner, tree never blanks.
    expect(screen.getAllByTestId("files-skeleton").length).toBeGreaterThan(0);

    // Retry now refetches IMMEDIATELY — no waiting out the back-off timer.
    const before = spy.mock.calls.length;
    fireEvent.click(screen.getByTestId("files-retry-now"));
    await waitFor(() => expect(spy.mock.calls.length).toBeGreaterThan(before));
    // Still retrying (server is still down) — the budget restarts, no terminal yet.
    await waitFor(() => expect(screen.getByTestId("files-retrying")).toBeTruthy());
  });

  it("gives up after the 3-attempt budget into a role=alert terminal with [Try again] (ISI-5339 F03)", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const spy = vi.fn(() => Promise.resolve(retryable503()));
    vi.stubGlobal("fetch", spy as unknown as typeof fetch);
    render(<FileExplorerTab projectId="web" />);

    await waitFor(() => expect(screen.getByTestId("files-retrying")).toBeTruthy());
    expect(screen.getByText(/attempt 1 of 3/i)).toBeTruthy();

    await act(async () => { vi.advanceTimersByTime(2_100); });
    await waitFor(() => expect(screen.getByText(/attempt 2 of 3/i)).toBeTruthy());

    await act(async () => { vi.advanceTimersByTime(4_100); });
    await waitFor(() => expect(screen.getByText(/attempt 3 of 3/i)).toBeTruthy());

    await act(async () => { vi.advanceTimersByTime(8_100); });
    // Terminal: role=alert, human copy, manual Try again — never a bare 504/500.
    await waitFor(() => expect(screen.getByTestId("files-error")).toBeTruthy());
    expect(screen.getByRole("alert")).toBeTruthy();
    expect(screen.getByText("We couldn't load these files.")).toBeTruthy();
    expect(screen.getByTestId("files-try-again")).toBeTruthy();
    // Bounded: exactly the initial fetch + 3 budgeted attempts, no more.
    expect(spy.mock.calls.length).toBe(4);
  });

  // ISI-5355 reconciliation: a lazy expand can hit the same cold reader the
  // root does. The expand row follows the SAME bounded retry ladder — an
  // inline amber "retrying" row with the attempt budget and a manual
  // [Retry now] — instead of the misread terminal "Couldn't load this folder."
  it("child expand on a typed 503: inline retry row with [Retry now], refetch succeeds (ISI-5355)", async () => {
    let srcCalls = 0;
    vi.stubGlobal(
      "fetch",
      vi.fn((url: string) => {
        const u = new URL(url, "http://localhost");
        if (u.searchParams.get("path") === "src") {
          srcCalls += 1;
          if (srcCalls === 1) return Promise.resolve(retryable503());
          const json = () => Promise.resolve({ path: "src", entries: [{ name: "main.ts", path: "src/main.ts", type: "file", size: 2 }] });
          return Promise.resolve({ ok: true, status: 200, json, clone: () => ({ json }) } as unknown as Response);
        }
        const json = () => Promise.resolve({ path: "", entries: [{ name: "src", path: "src", type: "dir" }] });
        return Promise.resolve({ ok: true, status: 200, json, clone: () => ({ json }) } as unknown as Response);
      }) as unknown as typeof fetch,
    );
    render(<FileExplorerTab projectId="web" />);
    await waitFor(() => expect(screen.getByTestId("files-tree")).toBeTruthy());
    fireEvent.click(screen.getByText("src"));
    // Transient 503 → the inline amber retry row names the budget…
    await waitFor(() => expect(screen.getByTestId("files-dir-retrying")).toBeTruthy());
    expect(screen.getByText(/retrying/i)).toBeTruthy();
    expect(screen.getByText(/attempt 1 of 3/i)).toBeTruthy();
    // …never the terminal "couldn't load" copy for a transient failure.
    expect(screen.queryByTestId("files-dir-error")).toBeNull();
    // [Retry now] refetches the dir immediately and the children render.
    fireEvent.click(screen.getByTestId("files-dir-retry-now"));
    await waitFor(() => expect(screen.getByText("main.ts")).toBeTruthy());
    expect(srcCalls).toBe(2);
  });

  // ISI-5355 reconciliation: terminal per-row failure keeps a real retry path —
  // dirs[path] is already set, so without the [Retry] affordance a collapsed
  // re-expand can never reload the folder.
  it("child expand terminal failure: honest row + [Retry] refetches that dir (ISI-5355)", async () => {
    let srcCalls = 0;
    vi.stubGlobal(
      "fetch",
      vi.fn((url: string) => {
        const u = new URL(url, "http://localhost");
        if (u.searchParams.get("path") === "src") {
          srcCalls += 1;
          if (srcCalls === 1) {
            return Promise.resolve({ ok: false, status: 404, json: () => Promise.resolve(null) } as unknown as Response);
          }
          const json = () => Promise.resolve({ path: "src", entries: [{ name: "main.ts", path: "src/main.ts", type: "file", size: 2 }] });
          return Promise.resolve({ ok: true, status: 200, json, clone: () => ({ json }) } as unknown as Response);
        }
        const json = () => Promise.resolve({ path: "", entries: [{ name: "src", path: "src", type: "dir" }] });
        return Promise.resolve({ ok: true, status: 200, json, clone: () => ({ json }) } as unknown as Response);
      }) as unknown as typeof fetch,
    );
    render(<FileExplorerTab projectId="web" />);
    await waitFor(() => expect(screen.getByTestId("files-tree")).toBeTruthy());
    fireEvent.click(screen.getByText("src"));
    await waitFor(() => expect(screen.getByTestId("files-dir-error")).toBeTruthy());
    expect(screen.getByText(/Couldn't load this folder/i)).toBeTruthy();
    fireEvent.click(screen.getByTestId("files-dir-retry"));
    await waitFor(() => expect(screen.getByText("main.ts")).toBeTruthy());
    expect(srcCalls).toBe(2);
  });

  // ISI-4705: the S4b `/files` wire payload omits `path` on each entry. The tree
  // keys its open-state and lazy child listings by `path`; when every path is
  // `undefined` a single expand collapses ALL directories into one shared open +
  // one shared (root) child listing → infinite recursive render → UI crash.
  // normalizeListing derives per-entry paths so identity is restored.
  it("derives entry paths when the wire omits them, so an expand loads THAT dir — not the root (ISI-4705 crash)", async () => {
    // Note the entries carry NO `path` field, exactly like the live payload.
    routeFetch({
      listings: {
        "": { path: "", entries: [
          { name: "todo-app", type: "dir", size: 0 },
          { name: "README.md", type: "file", size: 5 },
        ] },
        "todo-app": { path: "todo-app", entries: [
          { name: "docs", type: "dir", size: 0 },
        ] },
      },
    });
    render(<FileExplorerTab projectId="web" />);
    await waitFor(() => expect(screen.getByTestId("file-explorer")).toBeTruthy());

    // Expanding todo-app must load todo-app's OWN children (docs), and must NOT
    // re-render the root listing under it (README.md stays a single root row).
    fireEvent.click(screen.getByText("todo-app"));
    await waitFor(() => expect(screen.getByText("docs")).toBeTruthy());
    // The bug rendered the root's README.md recursively under every open dir;
    // with derived paths it appears exactly once (the root row).
    expect(screen.getAllByText("README.md")).toHaveLength(1);
  });

  it("normalizeListing derives unique root-relative paths, ignoring any server-sent path (ISI-4705)", () => {
    const derived = normalizeListing("todo-app", {
      path: "todo-app",
      entries: [
        { name: "docs", type: "dir" },
        { name: "main.go", type: "file", size: 3 },
      ],
    });
    expect(derived.entries?.map((e) => e.path)).toEqual(["todo-app/docs", "todo-app/main.go"]);
    // The path is derived from dir+name and a server-supplied `path` is NOT
    // trusted — so even a server that emits a duplicate/wrong path cannot
    // reintroduce the shared-key recursion. Here both rows claim "dup" yet get
    // distinct derived keys.
    const adversarial = normalizeListing("", {
      path: "",
      entries: [
        { name: "a", path: "dup", type: "dir" },
        { name: "b", path: "dup", type: "dir" },
      ],
    });
    expect(adversarial.entries?.map((e) => e.path)).toEqual(["a", "b"]);
  });

  // ISI-4705: a capped read window can be up to 1 MiB; running highlight.js /
  // ReactMarkdown over that much on the main thread freezes and can OOM-crash the
  // tab. Above the rich-preview cap the preview degrades to download-instead.
  it("guards a large code file from the main-thread renderer, offering download instead (ISI-4705)", async () => {
    const big = "x".repeat(300 * 1024); // > RICH_PREVIEW_MAX_BYTES (256 KiB)
    routeFetch({
      listings: { "": { path: "", entries: [{ name: "huge.go", path: "huge.go", type: "file", size: 3_850_240 }] } },
      contents: { "huge.go": { path: "huge.go", size: 3_850_240, contentType: "text", data: b64(big), offset: 0, length: big.length } },
    });
    render(<FileExplorerTab projectId="web" />);
    await waitFor(() => expect(screen.getByTestId("file-explorer")).toBeTruthy());
    fireEvent.click(screen.getByText("huge.go"));
    await waitFor(() => expect(screen.getByTestId("files-too-large")).toBeTruthy());
    // The expensive code renderer is NOT mounted for an oversized window.
    expect(screen.queryByTestId("files-code")).toBeNull();
    expect(screen.getByTestId("files-too-large-download").getAttribute("href")).toContain("huge.go");
    // A capped read (length < size) is always flagged truncated, even though the
    // server omits the `truncated` field.
    expect(screen.getByTestId("files-preview-truncated")).toBeTruthy();
  });

  // ISI-4705: the /files/content wire payload omits `path` (just like the listing).
  // The preview reached for c.path immediately (previewKind → fileExt → path.slice),
  // so opening a file threw "Cannot read properties of undefined (reading 'slice')"
  // and tripped the error boundary. normalizeContent backfills the requested path.
  it("previews a file whose content payload omits `path`, without crashing (ISI-4705)", async () => {
    routeFetch({
      listings: { "": { path: "", entries: [{ name: "01-plan.md", type: "file", size: 12 }] } },
      // NOTE: no `path` on the content, exactly like the live reader response.
      contents: {
        "01-plan.md": {
          size: 12,
          contentType: "text",
          data: b64("# Plan\n\nhi"),
          offset: 0,
          length: 12,
        } as WireFileContent,
      },
    });
    render(<FileExplorerTab projectId="web" />);
    await waitFor(() => expect(screen.getByTestId("file-explorer")).toBeTruthy());
    fireEvent.click(screen.getByText("01-plan.md"));
    // The markdown preview renders — not the error-boundary panel.
    await waitFor(() => expect(screen.getByTestId("files-markdown")).toBeTruthy());
    expect(screen.queryByTestId("files-honest")).toBeNull();
    // The path was backfilled from the request, so the preview header shows it.
    expect(screen.getByTestId("files-preview-path").textContent).toBe("01-plan.md");
  });

  it("normalizeContent backfills the requested path when the wire omits it (ISI-4705)", () => {
    const c = normalizeContent("docs/01-plan.md", {
      size: 3,
      contentType: "text",
      data: "eA==",
      offset: 0,
      length: 3,
    } as WireFileContent);
    expect(c.path).toBe("docs/01-plan.md");
    // A server that DOES echo a path is preserved.
    const kept = normalizeContent("req/path", { path: "real/path", size: 0, contentType: "text", data: "", offset: 0, length: 0 } as FileContent);
    expect(kept.path).toBe("real/path");
  });

  // ISI-4705 (PR #530 review): pin the telemetry deliverables — without these,
  // the boundary and the reporter could be deleted with the suite staying green.
  it("reportClientError emits a structured console line AND a server beacon (ISI-4705)", () => {
    const errSpy = vi.spyOn(console, "error").mockImplementation(() => {});
    const beacon = vi.fn((_url: string, _body?: BodyInit) => true);
    vi.stubGlobal("navigator", { sendBeacon: beacon, userAgent: "vitest" });

    reportClientError("unit-test", new Error("kaboom"), { projectId: "web" });

    expect(errSpy).toHaveBeenCalledWith(
      "[client-crash]",
      expect.objectContaining({ source: "unit-test", message: "kaboom", context: { projectId: "web" } }),
    );
    expect(beacon).toHaveBeenCalledTimes(1);
    expect(beacon.mock.calls[0][0]).toBe("/api/telemetry/client-error");
  });

  it("the error boundary catches a render throw, shows an honest panel, and reports it (ISI-4705)", () => {
    const errSpy = vi.spyOn(console, "error").mockImplementation(() => {});
    const beacon = vi.fn(() => true);
    vi.stubGlobal("navigator", { sendBeacon: beacon, userAgent: "vitest" });
    const Boom = (): never => {
      throw new Error("render exploded");
    };

    render(
      <FileExplorerErrorBoundary projectId="web">
        <Boom />
      </FileExplorerErrorBoundary>,
    );

    // Honest, contained panel — NOT a white-screen propagating to the root.
    expect(screen.getByTestId("files-honest")).toBeTruthy();
    // And the crash was reported through the collectable channel.
    expect(errSpy).toHaveBeenCalledWith(
      "[client-crash]",
      expect.objectContaining({ source: "file-explorer" }),
    );
  });

  it("gives each file type its own icon tone, and folders never take a file tone (ISI-4747 follow-up)", async () => {
    routeFetch({
      listings: {
        "": {
          path: "",
          entries: [
            { name: "src", path: "src", type: "dir" },
            { name: "main.go", path: "main.go", type: "file", size: 10 },
            { name: "app.py", path: "app.py", type: "file", size: 10 },
            { name: "index.js", path: "index.js", type: "file", size: 10 },
            { name: "Main.java", path: "Main.java", type: "file", size: 10 },
            { name: "Program.cs", path: "Program.cs", type: "file", size: 10 },
            { name: "notes.md", path: "notes.md", type: "file", size: 10 },
            { name: "report.docx", path: "report.docx", type: "file", size: 10 },
            { name: "logo.png", path: "logo.png", type: "file", size: 10 },
            { name: "data.json", path: "data.json", type: "file", size: 10 },
            { name: "bundle.zip", path: "bundle.zip", type: "file", size: 10 },
            { name: "clip.mp4", path: "clip.mp4", type: "file", size: 10 },
            { name: "Makefile", path: "Makefile", type: "file", size: 10 },
          ],
        },
      },
    });
    render(<FileExplorerTab projectId="web" />);
    await waitFor(() => expect(screen.getByTestId("file-explorer")).toBeTruthy());

    // Each recognised extension paints its glyph a distinct tone class...
    const toneOf = (label: string): string => {
      const glyph = screen.getByText(label).parentElement?.querySelector(".file-explorer__glyph--file");
      return glyph?.getAttribute("class") ?? "";
    };
    expect(toneOf("main.go")).toContain("file-explorer__glyph--go");
    expect(toneOf("app.py")).toContain("file-explorer__glyph--py");
    expect(toneOf("index.js")).toContain("file-explorer__glyph--js");
    expect(toneOf("Main.java")).toContain("file-explorer__glyph--java");
    expect(toneOf("Program.cs")).toContain("file-explorer__glyph--cs");
    expect(toneOf("notes.md")).toContain("file-explorer__glyph--doc");
    expect(toneOf("report.docx")).toContain("file-explorer__glyph--docx");
    expect(toneOf("logo.png")).toContain("file-explorer__glyph--image");
    expect(toneOf("data.json")).toContain("file-explorer__glyph--data");
    expect(toneOf("bundle.zip")).toContain("file-explorer__glyph--archive");
    expect(toneOf("clip.mp4")).toContain("file-explorer__glyph--media");
    // ...and an extension-less file still gets a glyph — the muted default, never nothing.
    expect(toneOf("Makefile")).toContain("file-explorer__glyph--default");

    // A directory carries the accent folder glyph, never a file tone.
    const dirGlyph = screen.getByText("src").parentElement?.querySelector(".file-explorer__glyph--dir");
    expect(dirGlyph).toBeTruthy();
    expect(screen.getByText("src").parentElement?.querySelector(".file-explorer__glyph--file")).toBeNull();
  });
});

// ISI-5485 (ADR-0025 D5a, child B) — cache-first render + background revalidate
// + in-place reconcile. A controllable fetch lets us assert the tree paints from
// the durable session cache BEFORE the network resolves, then reconciles.
describe("FileExplorerTab — D5a cache-first (ISI-5485)", () => {
  /** A fetch whose root (`path=""`) listing resolves only when we call release(),
   * so the cache-first paint can be asserted with the network still pending. */
  function deferredRootFetch(resolveWith: () => Response) {
    let release!: () => void;
    const gate = new Promise<void>((r) => (release = r));
    const spy = vi.fn(async (url: string) => {
      const u = new URL(url, "http://localhost");
      const path = u.searchParams.get("path") ?? "";
      if (u.pathname.endsWith("/files") && path === "") {
        await gate;
        return resolveWith();
      }
      return { ok: false, status: 404, json: () => Promise.resolve(null) } as Response;
    });
    vi.stubGlobal("fetch", spy as unknown as typeof fetch);
    return { spy, release };
  }
  const resp = (body: unknown, status = 200): Response =>
    ({ ok: status >= 200 && status < 300, status, json: () => Promise.resolve(body) }) as unknown as Response;

  it("renders the tree synchronously from cache with no blocking spinner + a Syncing… chip (AC1)", async () => {
    cacheListing("web", "", {
      path: "",
      generation: "g1",
      entries: [
        { name: "src", path: "src", type: "dir" },
        { name: "README.md", path: "README.md", type: "file", size: 12 },
      ],
    });
    // Network is held open → the only way the rows can appear is the cache.
    deferredRootFetch(() => resp({ path: "", entries: [], generation: "g1" }));

    render(<FileExplorerTab projectId="web" />);

    await waitFor(() => expect(screen.getByText("README.md")).toBeTruthy());
    expect(screen.getByText("src")).toBeTruthy();
    // No full-tab loading skeleton took over, and the background-sync chip shows.
    expect(screen.queryByTestId("files-loading")).toBeNull();
    expect(screen.getByTestId("files-sync-syncing")).toBeTruthy();
  });

  it("reconciles fresh same-generation bytes in place and flips to Up to date (AC1)", async () => {
    cacheListing("web", "", {
      path: "",
      generation: "g1",
      entries: [
        { name: "keep.ts", path: "keep.ts", type: "file", size: 1 },
        { name: "gone.ts", path: "gone.ts", type: "file", size: 1 },
      ],
    });
    const { release } = deferredRootFetch(() =>
      resp({
        path: "",
        generation: "g1",
        entries: [
          { name: "keep.ts", type: "file", size: 2 },
          { name: "new.ts", type: "file", size: 3 },
        ],
      }),
    );

    render(<FileExplorerTab projectId="web" />);
    await waitFor(() => expect(screen.getByText("gone.ts")).toBeTruthy()); // cached first

    await act(async () => {
      release();
      await Promise.resolve();
    });

    await waitFor(() => expect(screen.getByTestId("files-sync-updated")).toBeTruthy());
    expect(screen.getByText("new.ts")).toBeTruthy();
    expect(screen.queryByText("gone.ts")).toBeNull(); // removed in place
    expect(screen.getByText("keep.ts")).toBeTruthy(); // survived (restated)
  });

  it("a generation change drops stale bytes and replaces the tree wholesale (AC2 / C3)", async () => {
    cacheListing("web", "", {
      path: "",
      generation: "g1",
      entries: [{ name: "old.ts", path: "old.ts", type: "file", size: 1 }],
    });
    const { release } = deferredRootFetch(() =>
      resp({ path: "", generation: "g2", entries: [{ name: "fresh.ts", type: "file", size: 1 }] }),
    );

    render(<FileExplorerTab projectId="web" />);
    await waitFor(() => expect(screen.getByText("old.ts")).toBeTruthy()); // stale G1 from cache

    await act(async () => {
      release();
      await Promise.resolve();
    });

    await waitFor(() => expect(screen.getByText("fresh.ts")).toBeTruthy());
    expect(screen.queryByText("old.ts")).toBeNull(); // stale bytes never survive the epoch flip
    expect(screen.getByTestId("files-sync-updated")).toBeTruthy();
  });

  it("a failed background revalidate keeps the cached tree and shows Showing cached (AC3)", async () => {
    cacheListing("web", "", {
      path: "",
      generation: "g1",
      entries: [{ name: "keep.ts", path: "keep.ts", type: "file", size: 1 }],
    });
    const { release } = deferredRootFetch(() => resp(null, 503));

    render(<FileExplorerTab projectId="web" />);
    await waitFor(() => expect(screen.getByText("keep.ts")).toBeTruthy());

    await act(async () => {
      release();
      await Promise.resolve();
    });

    await waitFor(() => expect(screen.getByTestId("files-sync-stale")).toBeTruthy());
    expect(screen.getByText("keep.ts")).toBeTruthy(); // degraded, not blank
  });
});
