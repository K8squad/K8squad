// test/projects/FileExplorerTab.test.tsx — ISI-3956 S4c: the File Explorer tab.
// Covers AC1 tree lazy-expand + read-only preview (no mutate affordance),
// AC2 binary placeholder (no garbled text), AC3 loading, AC4 workspace-busy
// degraded banner over the snapshot, AC5 empty + 501, and 404 existence-hiding.

import { describe, it, expect, afterEach, vi } from "vitest";
import { render, screen, cleanup, waitFor, fireEvent, within } from "@testing-library/react";
import { FileExplorerTab } from "@/components/FileExplorerTab";
import type { FileContent, FileListing } from "@/lib/project-files";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

/** base64 of a UTF-8 string (jsdom has Buffer). */
const b64 = (s: string) => Buffer.from(s, "utf-8").toString("base64");

/** Route the two BFF endpoints to per-path canned responses. `listings` is keyed
 * by the `path` query (root = ""); `contents` by the file path. A key that maps
 * to a number is returned as that HTTP status with a null body. */
function routeFetch(opts: {
  listings?: Record<string, FileListing | number>;
  contents?: Record<string, FileContent | number>;
  status?: number;
}) {
  const spy = vi.fn((url: string) => {
    const u = new URL(url, "http://localhost");
    const isContent = u.pathname.endsWith("/files/content");
    const path = u.searchParams.get("path") ?? "";
    let entry: unknown = opts.status;
    if (isContent) entry = opts.contents?.[path];
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
    await waitFor(() => expect(screen.getByTestId("files-text")).toBeTruthy());
    expect(screen.getByTestId("files-text").textContent).toBe("export const x=1");

    // Read-only (§D3): no edit/save/delete/rename/run affordance anywhere.
    const root = screen.getByTestId("file-explorer");
    for (const forbidden of [/save/i, /delete/i, /rename/i, /\brun\b/i, /edit/i]) {
      expect(within(root).queryByText(forbidden)).toBeNull();
    }
  });

  it("shows a binary placeholder with byte count, never garbled text (AC2)", async () => {
    routeFetch({
      listings: { "": { path: "", entries: [{ name: "logo.png", path: "logo.png", type: "file", size: 2048 }] } },
      contents: { "logo.png": { path: "logo.png", size: 2048, contentType: "binary", data: b64("\x89PNG\r\n"), offset: 0, length: 2048 } },
    });
    render(<FileExplorerTab projectId="web" />);
    await waitFor(() => expect(screen.getByTestId("file-explorer")).toBeTruthy());
    fireEvent.click(screen.getByText("logo.png"));
    await waitFor(() => expect(screen.getByTestId("files-binary")).toBeTruthy());
    const binary = screen.getByTestId("files-binary");
    expect(within(binary).getByText(/Binary file/)).toBeTruthy();
    expect(within(binary).getByText(/2\.0 KB/)).toBeTruthy();
    // Never rendered as a text <pre>.
    expect(screen.queryByTestId("files-text")).toBeNull();
    // Read-only download of raw bytes is a data: URL (no write path into the volume).
    expect(screen.getByTestId("files-binary-download").getAttribute("href")).toMatch(/^data:application\/octet-stream;base64,/);
  });

  it("renders a loading state while the root tree is fetching (AC3)", () => {
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
