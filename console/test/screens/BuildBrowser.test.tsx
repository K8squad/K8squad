import { describe, it, expect, afterEach, vi } from "vitest";
import { render, screen, cleanup, waitFor } from "@testing-library/react";
import { BuildBrowser, classifyBuildStatus, decodeContent } from "@/components/BuildBrowser";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

const meta = { runId: "run-1", head: "abc123def456aa", base: "000111222333bb", changedFiles: 2 };
const tree = {
  ref: "run",
  entries: [
    { path: "main.go", mode: "100644", size: 120 },
    { path: "internal/x.go", mode: "100644", size: 44 },
  ],
  truncated: false,
};
const diff = { base: "000111", head: "abc123", patch: "--- a/main.go\n+++ b/main.go\n@@ -1 +1 @@\n-old\n+new\n", truncated: false };

// The screen fires meta+tree+diff in parallel on mount; each test's fetch stub answers by URL so
// the three initial calls resolve independently (order-agnostic).
function stubByUrl(map: Record<string, { ok: boolean; status: number; json: () => Promise<unknown> }>) {
  vi.stubGlobal(
    "fetch",
    vi.fn().mockImplementation((url: string) => {
      for (const key of Object.keys(map)) {
        if (url.includes(key)) return Promise.resolve(map[key]);
      }
      return Promise.resolve({ ok: false, status: 500, json: () => Promise.resolve(null) });
    }),
  );
}

const ok = (body: unknown) => ({ ok: true, status: 200, json: () => Promise.resolve(body) });
const fail = (status: number) => ({ ok: false, status, json: () => Promise.resolve(null) });

describe("<BuildBrowser> — story 8.7e three-pane wiring (ISI-2904)", () => {
  it("fetches meta+tree+diff and renders all three panes, diff-first", async () => {
    stubByUrl({ "/meta": ok(meta), "/tree": ok(tree), "/diff": ok(diff) });
    render(<BuildBrowser runId="run-1" />);
    await waitFor(() => expect(screen.getByTestId("build-ready")).toBeTruthy());
    expect(screen.getByTestId("build-meta").textContent).toContain("2 changed files");
    expect(screen.getAllByTestId("build-tree-entry").length).toBe(2);
    // Diff is the default viewer.
    expect(screen.getByTestId("build-diff").textContent).toContain("+new");
    expect(screen.queryByTestId("build-file")).toBeNull();
  });

  it("selecting a tree entry loads that file's bytes and switches the viewer to file mode", async () => {
    const fileBody = "package main\n";
    stubByUrl({
      "/meta": ok(meta),
      "/tree": ok(tree),
      "/diff": ok(diff),
      "/build/file": ok({ ref: "run", path: "main.go", content: btoa(fileBody), size: fileBody.length, truncated: false }),
    });
    render(<BuildBrowser runId="run-1" />);
    await waitFor(() => expect(screen.getAllByTestId("build-tree-entry").length).toBe(2));
    (screen.getAllByTestId("build-tree-entry")[0] as HTMLButtonElement).click();
    await waitFor(() => expect(screen.getByTestId("build-file")).toBeTruthy());
    expect(screen.getByTestId("build-file").textContent).toContain("package main");
    expect(screen.getByTestId("build-viewer-file-label").textContent).toContain("main.go");
    // The Diff tab returns to the unified patch.
    (screen.getByTestId("build-tab-diff") as HTMLButtonElement).click();
    await waitFor(() => expect(screen.getByTestId("build-diff")).toBeTruthy());
  });

  it("surfaces the file truncation flag (server cap)", async () => {
    stubByUrl({
      "/meta": ok(meta),
      "/tree": ok(tree),
      "/diff": ok(diff),
      "/build/file": ok({ ref: "run", path: "main.go", content: btoa("partial"), size: 999999, truncated: true }),
    });
    render(<BuildBrowser runId="run-1" />);
    await waitFor(() => expect(screen.getAllByTestId("build-tree-entry").length).toBe(2));
    (screen.getAllByTestId("build-tree-entry")[0] as HTMLButtonElement).click();
    await waitFor(() => expect(screen.getByTestId("build-file")).toBeTruthy());
    expect(screen.getByTestId("build-viewer-file-label").textContent).toContain("truncated at server cap");
  });

  it("renders the empty-diff card when there are no changes", async () => {
    stubByUrl({ "/meta": ok(meta), "/tree": ok(tree), "/diff": ok({ ...diff, patch: "" }) });
    render(<BuildBrowser runId="run-1" />);
    await waitFor(() => expect(screen.getByTestId("build-diff-empty")).toBeTruthy());
  });

  it("renders the SAME 404 card for missing and denied Runs (existence-hiding)", async () => {
    stubByUrl({ "/meta": fail(404), "/tree": fail(404), "/diff": fail(404) });
    render(<BuildBrowser runId="run-x" />);
    await waitFor(() => expect(screen.getByTestId("build-not-found")).toBeTruthy());
  });

  it("renders the not-wired card on the documented 501", async () => {
    stubByUrl({ "/meta": fail(501), "/tree": fail(501), "/diff": fail(501) });
    render(<BuildBrowser runId="run-1" />);
    await waitFor(() => expect(screen.getByTestId("build-not-wired")).toBeTruthy());
  });

  it("renders the unauthenticated card on 401", async () => {
    stubByUrl({ "/meta": fail(401), "/tree": fail(401), "/diff": fail(401) });
    render(<BuildBrowser runId="run-1" />);
    await waitFor(() => expect(screen.getByTestId("build-unauthenticated")).toBeTruthy());
  });

  it("discards a stale file response when a newer selection supersedes it", async () => {
    let resolveFirst: (v: { ok: boolean; status: number; json: () => Promise<unknown> }) => void = () => {};
    const firstClick = new Promise<{ ok: boolean; status: number; json: () => Promise<unknown> }>((res) => {
      resolveFirst = res;
    });
    let fileCall = 0;
    vi.stubGlobal(
      "fetch",
      vi.fn().mockImplementation((url: string) => {
        if (url.includes("/meta")) return Promise.resolve(ok(meta));
        if (url.includes("/tree")) return Promise.resolve(ok(tree));
        if (url.includes("/diff")) return Promise.resolve(ok(diff));
        // /build/file
        fileCall++;
        if (fileCall === 1) return firstClick; // first file — deliberately stalled
        return Promise.resolve(
          ok({ ref: "run", path: "internal/x.go", content: btoa("second-file"), size: 11, truncated: false }),
        );
      }),
    );
    render(<BuildBrowser runId="run-1" />);
    await waitFor(() => expect(screen.getAllByTestId("build-tree-entry").length).toBe(2));
    const entries = screen.getAllByTestId("build-tree-entry") as HTMLButtonElement[];
    entries[0].click(); // main.go — stalled
    await waitFor(() => expect(entries[1].disabled).toBe(false));
    entries[1].click(); // internal/x.go — completes first
    await waitFor(() => expect(screen.getByTestId("build-file").textContent).toContain("second-file"));
    // The stale first response now lands — it must NOT overwrite the viewer.
    resolveFirst(ok({ ref: "run", path: "main.go", content: btoa("first-file"), size: 10, truncated: false }));
    await new Promise((r) => setTimeout(r, 25));
    expect(screen.getByTestId("build-file").textContent).toContain("second-file");
    expect(screen.getByTestId("build-file").textContent).not.toContain("first-file");
  });
});

describe("<BuildBrowser> — story 8.7g PR/CI header strip", () => {
  it("renders the PR link and CI status when the SCM mirror has synced them", async () => {
    stubByUrl({
      "/meta": ok({ ...meta, prUrl: "https://github.com/K8squad/K8squad/pull/140", ciStatus: "passing" }),
      "/tree": ok(tree),
      "/diff": ok(diff),
    });
    render(<BuildBrowser runId="run-1" />);
    await waitFor(() => expect(screen.getByTestId("build-ready")).toBeTruthy());
    const strip = screen.getByTestId("build-pr-ci-strip");
    expect(strip.querySelector('[data-testid="build-pr-link"]')?.getAttribute("href")).toBe(
      "https://github.com/K8squad/K8squad/pull/140",
    );
    expect(screen.getByTestId("build-ci-status").textContent).toContain("passing");
  });

  it("omits the strip entirely when no PR/CI is synced (git-only degradation, no Epic 11 dep)", async () => {
    stubByUrl({ "/meta": ok(meta), "/tree": ok(tree), "/diff": ok(diff) });
    render(<BuildBrowser runId="run-1" />);
    await waitFor(() => expect(screen.getByTestId("build-ready")).toBeTruthy());
    expect(screen.queryByTestId("build-pr-ci-strip")).toBeNull();
  });

  it("renders only the CI badge when a Run has CI state but no PR yet", async () => {
    stubByUrl({ "/meta": ok({ ...meta, ciStatus: "running" }), "/tree": ok(tree), "/diff": ok(diff) });
    render(<BuildBrowser runId="run-1" />);
    await waitFor(() => expect(screen.getByTestId("build-ready")).toBeTruthy());
    expect(screen.getByTestId("build-pr-ci-strip")).toBeTruthy();
    expect(screen.queryByTestId("build-pr-link")).toBeNull();
    expect(screen.getByTestId("build-ci-status").textContent).toContain("running");
  });
});

describe("classifyBuildStatus / decodeContent — unit contract", () => {
  it("maps relayed statuses to distinct honest states (matching the 8.3 read model)", () => {
    expect(classifyBuildStatus(401).kind).toBe("unauthenticated");
    expect(classifyBuildStatus(404).kind).toBe("not-found");
    expect(classifyBuildStatus(501).kind).toBe("not-wired");
    expect(classifyBuildStatus(503).kind).toBe("error");
  });

  it("decodes base64 content and degrades undecodable bytes visibly", () => {
    expect(decodeContent(btoa("hello"))).toBe("hello");
    expect(decodeContent("!!!not-base64!!!")).toBe("(undecodable content)");
  });
});
