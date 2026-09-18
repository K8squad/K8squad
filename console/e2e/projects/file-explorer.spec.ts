// ISI-4653 · File Explorer browse / preview / download — the browser leg (Playwright).
//
// Verifies the ISI-4230 acceptance criteria end-to-end against a seeded workspace, through
// the BFF choke point (browser → Next BFF → apiserver), never the apiserver directly:
//   - browse the tree, with lazy directory expansion (AC1)
//   - type-aware preview of every file kind the viewer promises: code (go/json/yaml), markdown,
//     image (svg/png) (ISI-4648)
//   - download a file and a folder as a tar.gz archive (ISI-4652 / ISI-4650)
//   - the honest degraded ("workspace busy") and empty ("no files yet") states (AC4 / AC5)
//
// Companion deterministic proof: test/projects/FileExplorerTab.test.tsx pins every honest state
// and the type-aware preview at the component boundary with canned wire responses. This spec is
// the live-browser leg — it proves the user-visible journey over real HTTP, not a stubbed fetch.
//
// Seed contract (the live fixture workspace for KSQUAD_E2E_PROJECT_ID must contain these at its
// root; adjust the names below if the fixture differs — the assertions are by name + test-id):
//   main.go, README.md, config.json, values.yaml, logo.svg, logo.png, src/nested.ts (a directory)
//
// Convention (mirrors e2e/projects/project-id-roundtrip.spec.ts): source-scaffolded,
// skipped-with-reason until a live console + a seeded workspace is reachable. Set
// KSQUAD_CONSOLE_E2E=1 (and KSQUAD_E2E_PROJECT_ID) to activate; never silently dropped.
// Locators prefer the component's data-testid (files-*) where it is the clearest signal, with
// semantic getByRole for the tree rows — the same boundary the unit suite already exercises.

import { test, expect } from "@playwright/test";

const LIVE = process.env.KSQUAD_CONSOLE_E2E === "1";
const SKIP_REASON =
  "ISI-4653 File Explorer e2e: set KSQUAD_CONSOLE_E2E=1 against a live console with a " +
  "namespace-qualified Project whose workspace is seeded (see seed contract above).";

// A namespace-qualified Project id — the shape the whole File Explorer reads (ns/name).
const PROJECT_ID =
  process.env.KSQUAD_E2E_PROJECT_ID ?? "bmad-squad/bmad-demo-project";
const encoded = encodeURIComponent(PROJECT_ID); // exactly one layer: "ns%2Fname"
const filesUrl = `/projects/${encoded}/files`;

// The workspace-root-relative paths the seed contract promises (see header).
const SEED = {
  code: "main.go",
  markdown: "README.md",
  json: "config.json",
  yaml: "values.yaml",
  svg: "logo.svg",
  png: "logo.png",
  dir: "src",
  nestedCode: "src/nested.ts",
} as const;

test.describe("ISI-4653 · File Explorer browse / preview / download", () => {
  test.skip(!LIVE, SKIP_REASON);

  test("browses the tree and lazily expands a directory (AC1)", async ({
    page,
  }) => {
    await page.goto(filesUrl);

    // The tree renders with the seeded root entries (directories first, then files).
    const tree = page.getByTestId("files-tree");
    await expect(tree).toBeVisible();
    await expect(tree.getByRole("button", { name: SEED.dir })).toBeVisible();
    await expect(
      tree.getByRole("button", { name: SEED.markdown }),
    ).toBeVisible();

    // The nested file is NOT in the DOM until the directory is expanded (lazy).
    await expect(tree.getByRole("button", { name: "nested.ts" })).toHaveCount(
      0,
    );

    await tree.getByRole("button", { name: SEED.dir }).click();
    await expect(tree.getByRole("button", { name: "nested.ts" })).toBeVisible();
  });

  test("previews a Go file as highlighted code (ISI-4648)", async ({
    page,
  }) => {
    await page.goto(filesUrl);
    await page.getByRole("button", { name: SEED.code }).click();

    const code = page.getByTestId("files-code");
    await expect(code).toBeVisible();
    // Syntax highlighting produced real token spans, and the source is preserved.
    await expect(code.locator(".hljs-keyword")).toBeAttached();
  });

  test("previews a markdown file as formatted content (ISI-4648)", async ({
    page,
  }) => {
    await page.goto(filesUrl);
    await page.getByRole("button", { name: SEED.markdown }).click();

    const md = page.getByTestId("files-markdown");
    await expect(md).toBeVisible();
    // Rendered rich text — at least one heading or block element, not a raw <pre>.
    await expect(md.locator("h1, h2, h3, strong, em").first()).toBeAttached();
    await expect(page.getByTestId("files-text")).toHaveCount(0);
  });

  test("previews JSON and YAML as highlighted code (ISI-4648)", async ({
    page,
  }) => {
    await page.goto(filesUrl);

    await page.getByRole("button", { name: SEED.json }).click();
    await expect(page.getByTestId("files-code")).toBeVisible();

    await page.getByRole("button", { name: SEED.yaml }).click();
    await expect(page.getByTestId("files-code")).toBeVisible();
  });

  test("previews an SVG as an image with the svg mime (ISI-4648)", async ({
    page,
  }) => {
    await page.goto(filesUrl);
    await page.getByRole("button", { name: SEED.svg }).click();

    const img = page.getByTestId("files-image").locator("img");
    await expect(img).toBeVisible();
    await expect(img).toHaveAttribute("src", /^data:image\/svg\+xml;base64,/);
    // An image is never the binary placeholder nor garbled text.
    await expect(page.getByTestId("files-binary")).toHaveCount(0);
    await expect(page.getByTestId("files-text")).toHaveCount(0);
  });

  test("previews a PNG as an image with the png mime (ISI-4648)", async ({
    page,
  }) => {
    await page.goto(filesUrl);
    await page.getByRole("button", { name: SEED.png }).click();

    const img = page.getByTestId("files-image").locator("img");
    await expect(img).toBeVisible();
    await expect(img).toHaveAttribute("src", /^data:image\/png;base64,/);
    await expect(page.getByTestId("files-binary")).toHaveCount(0);
  });

  test("downloads a file from the per-row anchor (ISI-4652)", async ({
    page,
  }) => {
    await page.goto(filesUrl);

    const rowDownload = page.getByTestId("files-download-file").first();
    await expect(rowDownload).toHaveAttribute(
      "href",
      new RegExp(
        `/api/projects/${encoded}/files/download\\?path=${SEED.markdown}`,
      ),
    );
    await expect(rowDownload).toHaveAttribute(
      "aria-label",
      `Download ${SEED.markdown}`,
    );

    // Live browser proof: the anchor resolves to an attachment named after the file.
    const dlPromise = page.waitForEvent("download");
    await rowDownload.click();
    const dl = await dlPromise;
    expect(dl.suggestedFilename()).toBe(SEED.markdown);
  });

  test("downloads a folder as a tar.gz archive (ISI-4650 / ISI-4652)", async ({
    page,
  }) => {
    await page.goto(filesUrl);

    const dirDownload = page.getByTestId("files-download-dir").first();
    await expect(dirDownload).toHaveAttribute(
      "href",
      new RegExp(
        `/api/projects/${encoded}/files/download\\?path=${encodeURIComponent(SEED.dir)}`,
      ),
    );
    await expect(dirDownload).toHaveAttribute(
      "aria-label",
      `Download ${SEED.dir} as archive`,
    );

    // Live browser proof: the folder streams a server-built tar.gz archive.
    const dlPromise = page.waitForEvent("download");
    await dirDownload.click();
    const dl = await dlPromise;
    expect(dl.suggestedFilename()).toMatch(/\.tar\.gz$/);
  });

  test.describe("honest states (degraded / empty)", () => {
    test.skip(
      !process.env.KSQUAD_E2E_EMPTY_PROJECT_ID,
      "ISI-4653 empty state: set KSQUAD_E2E_EMPTY_PROJECT_ID to a project whose " +
        "workspace has no files to activate.",
    );

    test("renders 'no files yet' for an empty workspace (AC5)", async ({
      page,
    }) => {
      const emptyProject = encodeURIComponent(
        process.env.KSQUAD_E2E_EMPTY_PROJECT_ID!,
      );
      await page.goto(`/projects/${emptyProject}/files`);

      await expect(page.getByTestId("files-empty")).toBeVisible();
      await expect(page.getByText(/No files yet/)).toBeVisible();
    });
  });

  test.describe("degraded state", () => {
    test.skip(
      !process.env.KSQUAD_E2E_DEGRADED_PROJECT_ID,
      "ISI-4653 degraded state: set KSQUAD_E2E_DEGRADED_PROJECT_ID to a project whose " +
        "workspace is held by a running agent (busy) to activate.",
    );

    test("shows the workspace-busy banner over the snapshot (AC4)", async ({
      page,
    }) => {
      const busyProject = encodeURIComponent(
        process.env.KSQUAD_E2E_DEGRADED_PROJECT_ID!,
      );
      await page.goto(`/projects/${busyProject}/files`);

      // The banner renders AND the snapshot rows still render — busy is informational,
      // never a blank error.
      await expect(page.getByTestId("files-busy-banner")).toBeVisible();
      await expect(page.getByTestId("files-tree")).toBeVisible();
    });
  });
});
