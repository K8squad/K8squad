// test/projects/FileExplorerTab.test.tsx — ISI-3956 S4c: the File Explorer tab.
// Covers AC1 tree lazy-expand + read-only preview (no mutate affordance),
// AC2 binary placeholder (no garbled text), AC3 loading, AC4 workspace-busy
// degraded banner over the snapshot, AC5 empty + 501, and 404 existence-hiding.

import { describe, it, expect, afterEach, vi } from "vitest";
import { render, screen, cleanup, waitFor, fireEvent, within } from "@testing-library/react";
import { FileExplorerTab } from "@/components/FileExplorerTab";
import type { FileContent, FileListing, FileStat } from "@/lib/project-files";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

/** base64 of a UTF-8 string (jsdom has Buffer). */
const b64 = (s: string) => Buffer.from(s, "utf-8").toString("base64");

/** Route the BFF endpoints to per-path canned responses. `listings` is keyed
 * by the `path` query (root = ""); `contents` and `stats` by the file path.
 * A key that maps to a number is returned as that HTTP status with a null body. */
function routeFetch(opts: {
  listings?: Record<string, FileListing | number>;
  contents?: Record<string, FileContent | number>;
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

  it("shows the workspace-busy degraded banner over the last-committed snapshot (AC4)", async () => {
    routeFetch({
      listings: { "": { path: "", degraded: true, entries: [{ name: "app.go", path: "app.go", type: "file", size: 8 }] } },
    });
    render(<FileExplorerTab projectId="web" />);
    await waitFor(() => expect(screen.getByTestId("files-busy-banner")).toBeTruthy());
    expect(screen.getByText(/workspace busy/i)).toBeTruthy();
    // The snapshot rows still render — busy is informational, never a blank error.
    expect(screen.getByText("app.go")).toBeTruthy();
  });

  it("renders 'no files yet' for an empty workspace (AC5)", async () => {
    routeFetch({ listings: { "": { path: "", entries: [] } } });
    render(<FileExplorerTab projectId="web" />);
    await waitFor(() => expect(screen.getByTestId("files-empty")).toBeTruthy());
    expect(screen.getByText(/No files yet/)).toBeTruthy();
  });

  it("renders honest 'not available yet' on 501 (AC5)", async () => {
    routeFetch({ listings: { "": 501 } });
    render(<FileExplorerTab projectId="web" />);
    await waitFor(() => expect(screen.getByTestId("files-honest")).toBeTruthy());
    expect(screen.getByText(/not available yet/)).toBeTruthy();
  });

  it("renders an existence-hiding not-found on 404 (no leak)", async () => {
    routeFetch({ listings: { "": 404 } });
    render(<FileExplorerTab projectId="web" />);
    await waitFor(() => expect(screen.getByTestId("files-honest")).toBeTruthy());
    expect(screen.getByText(/No files/)).toBeTruthy();
    // Never leaks a distinguishing "forbidden" vs "missing" — 404 is uniform.
    expect(screen.queryByText(/forbidden|403|permission/i)).toBeNull();
  });
});
