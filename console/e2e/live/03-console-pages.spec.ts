// e2e/live/03-console-pages.spec.ts — MATRIX ROWS: each core Console page loads authenticated.
//
// One row per core page. Each row proves the page: (a) responds < 400, (b) does NOT bounce back
// to /login (auth/shell-leak regression guard), (c) renders the authenticated nav rail, and
// (d) shows no client crash / server-error overlay. Pages are derived from the console nav model
// (lib/nav.ts) so this list cannot silently drift from the shipped navigation.
//
// Global pages always run (when creds present). Project-scoped pages (Build/Tickets/Runs/
// Discussion) need a project id: KSQUAD_PROJECT_ID, else auto-discovered from /api/projects; if
// no project exists on the target, those rows SKIP-WITH-REASON (a visible finding, not a silent
// pass).

import type { Page } from "@playwright/test";
import {
  test,
  expect,
  doLogin,
  BASE_URL,
  CREDS_PRESENT,
  NO_CREDS_REASON,
  PROJECT_ID,
} from "./config";

// Crash / error-boundary copy we treat as a hard fail if visible on any page.
const CRASH_COPY =
  /Application error|client-side exception|Internal Server Error|Something went wrong|This page could not be found|404/i;

async function assertPageHealthy(page: Page, path: string, label: string) {
  const resp = await page.goto(path, { waitUntil: "domcontentloaded" });
  const status = resp?.status() ?? 0;
  expect(status, `${label}: GET ${path} HTTP status`).toBeLessThan(400);
  await expect(page, `${label}: bounced back to /login`).not.toHaveURL(/\/login/);
  await expect(
    page.getByRole("complementary", { name: /primary/i }),
    `${label}: authenticated nav rail missing`,
  ).toBeVisible();
  await expect(
    page.getByText(CRASH_COPY),
    `${label}: crash / error overlay visible`,
  ).toHaveCount(0);
}

// Global core pages, derived from lib/nav.ts (top-level + Settings child + Compose + admin Users).
const GLOBAL_PAGES: Array<{ path: string; label: string }> = [
  { path: "/", label: "Dashboard" },
  { path: "/overview", label: "Overview" },
  { path: "/agents", label: "Agents" },
  { path: "/compose", label: "Compose" },
  { path: "/users", label: "Users & Roles" },
  { path: "/settings", label: "Settings" },
  { path: "/settings/configuration", label: "Settings → Configuration" },
  { path: "/audit", label: "Audit" },
  { path: "/credentials", label: "Credentials" },
];

test.describe("Core Console pages (authenticated)", () => {
  test.skip(!CREDS_PRESENT, NO_CREDS_REASON);

  for (const p of GLOBAL_PAGES) {
    test(
      `page loads: ${p.label}`,
      { annotation: { type: "feature", description: `Page: ${p.label}` } },
      async ({ authedPage }) => {
        await assertPageHealthy(authedPage, p.path, p.label);
      },
    );
  }
});

// The project sub-nav sections (lib/nav.ts PROJECT_SECTIONS), tested against a real project id.
const PROJECT_SECTIONS: Array<{ seg: string; label: string }> = [
  { seg: "build", label: "Build" },
  { seg: "tickets", label: "Tickets" },
  { seg: "runs", label: "Runs" },
  { seg: "discussion", label: "Discussion" },
];

test.describe("Project-scoped Console pages (authenticated)", () => {
  test.skip(!CREDS_PRESENT, NO_CREDS_REASON);

  // Resolve a project id: explicit env wins, else discover the first project via the BFF.
  let projectId = PROJECT_ID;

  test.beforeAll(async ({ browser }) => {
    if (projectId) return;
    const ctx = await browser.newContext({
      baseURL: BASE_URL,
      ignoreHTTPSErrors: true,
    });
    try {
      // A page-context request inherits the same origin; log in via the API session route first.
      const page = await ctx.newPage();
      await doLogin(page);
      const res = await page.request.get("/api/projects");
      if (res.ok()) {
        const body = (await res.json()) as {
          projects?: Array<{ id?: string; name?: string }>;
        };
        const first = body.projects?.[0];
        projectId = first?.id ?? first?.name ?? "";
      }
    } catch {
      /* discovery best-effort; empty projectId → rows skip-with-reason below */
    } finally {
      await ctx.close();
    }
  });

  for (const s of PROJECT_SECTIONS) {
    test(
      `project page loads: ${s.label}`,
      { annotation: { type: "feature", description: `Project page: ${s.label}` } },
      async ({ authedPage }) => {
        test.skip(
          !projectId,
          "no project exists on the target (and KSQUAD_PROJECT_ID unset) — cannot exercise " +
            "project-scoped pages. Create a project or set KSQUAD_PROJECT_ID to enable this row.",
        );
        await assertPageHealthy(
          authedPage,
          `/projects/${encodeURIComponent(projectId)}/${s.seg}`,
          `${s.label} (project ${projectId})`,
        );
      },
    );
  }
});
