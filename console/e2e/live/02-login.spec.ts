// e2e/live/02-login.spec.ts — MATRIX ROWS: login render, default-admin login, login→app routing.
//
// Semantic locators only (getByRole / getByLabel / getByText) per the repo E2E convention — no CSS
// or test-id selectors. The login-render row needs no credentials; the default-admin-login and
// routing rows SKIP-WITH-REASON when KSQUAD_PASSWORD is unset (never silently dropped).

import {
  test,
  expect,
  doLogin,
  USERNAME,
  CREDS_PRESENT,
  NO_CREDS_REASON,
} from "./config";

test.describe("Login page render", () => {
  test(
    "the branded sign-in screen renders with username, password and a submit control",
    { annotation: { type: "feature", description: "Login page renders" } },
    async ({ page }) => {
      await page.goto("/login?next=%2F");
      // Brand + title (proves the shell-free welcome screen, not a 404-inside-shell leak).
      await expect(
        page.getByRole("heading", { name: /sign in/i }),
      ).toBeVisible();
      // The form fields, by their accessible names.
      await expect(page.getByLabel(/username/i)).toBeVisible();
      await expect(page.getByLabel(/password/i)).toBeVisible();
      await expect(
        page.getByRole("button", { name: /sign ?in|log ?in/i }),
      ).toBeVisible();
    },
  );

  test(
    "wrong credentials surface a non-enumerating error and stay on /login",
    { annotation: { type: "feature", description: "Login: bad creds rejected" } },
    async ({ page }) => {
      await doLogin(page, {
        username: "nobody-here-9c2f",
        password: "definitely-not-correct",
      });
      await expect(
        page.getByRole("alert").or(page.getByText(/invalid|incorrect/i)),
      ).toBeVisible();
      await expect(page).toHaveURL(/\/login/);
    },
  );
});

test.describe("Default-admin login", () => {
  test.skip(!CREDS_PRESENT, NO_CREDS_REASON);

  test(
    "the default admin can sign in and is routed into the app",
    { annotation: { type: "feature", description: "Default-admin login" } },
    async ({ page }) => {
      await doLogin(page);
      // Success leaves /login for the ?next= target ("/").
      await expect(page, "admin login should leave /login").not.toHaveURL(
        /\/login/,
        { timeout: 15_000 },
      );
    },
  );

  test(
    "login → app routing lands on the authenticated shell (nav rail present)",
    { annotation: { type: "feature", description: "Login → app routing" } },
    async ({ page }) => {
      await doLogin(page);
      await expect(page).not.toHaveURL(/\/login/, { timeout: 15_000 });
      // The authenticated ConsoleShell rail (aside aria-label="Primary") is present ONLY once
      // logged in — its presence proves the post-login redirect reached the operator shell.
      await expect(
        page.getByRole("complementary", { name: /primary/i }),
      ).toBeVisible();
      // A known global nav destination is offered.
      await expect(
        page.getByRole("link", { name: "Overview" }).first(),
      ).toBeVisible();
    },
  );
});

// Keep the imported USERNAME referenced so lint doesn't flag it — it documents the default identity
// and is used implicitly by doLogin(); an explicit soft assertion also proves the default resolves.
test.describe("Login parameterization", () => {
  test(
    "resolved username is non-empty (parameter wiring)",
    { annotation: { type: "feature", description: "Login: params resolve" } },
    async () => {
      expect(USERNAME.length).toBeGreaterThan(0);
    },
  );
});
