// §6.7.1 A5 · password reset — console login leg (Playwright, semantic locators).
//
// Ref: docs/bmad/05-testing-strategy.md §6.7.1 (r3, ADR-033), §8/§10.
// Companion to the apiserver-middleware suite in pkg/auth/a5_password_reset_test.go:
// the Go suite proves the server-side contract (single-use/time-boxed token, session
// invalidation, no-enumeration, argon2id, NFR-SEC3); this spec proves the user-visible
// login legs — that the browser experience reflects that contract.
//
// Source-scaffolding (§13): mechanism-concrete now, skipped-with-reason until the
// console source (Epic 15) lands. Set KSQUAD_CONSOLE_E2E=1 with a reachable console +
// argon2id `auth`-schema fixture to activate. Never silently dropped: `test.skip`
// keeps the case visible in the Playwright report.
//
// Convention: semantic locators only (getByRole / getByLabel / getByText) — no CSS or
// test-id selectors (§8/§10, persona E2E conventions). User-visible outcome assertions.

import { test, expect } from '@playwright/test';

const LIVE = process.env.KSQUAD_CONSOLE_E2E === '1';
const SKIP_REASON =
  '§6.7.1 A5 console login leg: pending console source (Epic 15, ADR-033). ' +
  'Set KSQUAD_CONSOLE_E2E=1 against a live console + argon2id auth-schema fixture to activate.';

// Fixture principal — seeded argon2id in the throwaway `auth` schema, no real creds.
const USER = 'amelia';
const OLD_PASSWORD = 'old-Correct-Horse-Battery-1!';
const NEW_PASSWORD = 'new-Tr0ub4dor-&3-reset!';

test.describe('§6.7.1 A5 · local-cred password reset (console login leg)', () => {
  test.skip(!LIVE, SKIP_REASON);

  test('reset request shows an identical, non-enumerating confirmation for any username', async ({ page }) => {
    // Existing account.
    await page.goto('/login');
    await page.getByRole('link', { name: /forgot|reset|recover/i }).click();
    await page.getByLabel(/username/i).fill(USER);
    await page.getByRole('button', { name: /reset|recover|send/i }).click();
    const existingMsg = await page
      .getByText(/if an account (exists|matches)|check your|we('| ha)ve sent/i)
      .innerText();

    // Non-existent account — the confirmation copy must be identical (no oracle).
    await page.goto('/login');
    await page.getByRole('link', { name: /forgot|reset|recover/i }).click();
    await page.getByLabel(/username/i).fill('nobody-here-9c2f');
    await page.getByRole('button', { name: /reset|recover|send/i }).click();
    const missingMsg = await page
      .getByText(/if an account (exists|matches)|check your|we('| ha)ve sent/i)
      .innerText();

    expect(missingMsg).toBe(existingMsg);
    // The UI must not reveal existence via a distinct "no such user" error either.
    await expect(page.getByText(/no such user|user not found|does not exist/i)).toHaveCount(0);
  });

  test('after reset: old session is signed out and the old password is rejected', async ({ page, context }) => {
    // Establish a live session with the old credential.
    await login(page, USER, OLD_PASSWORD);
    await expect(page.getByRole('button', { name: new RegExp(USER, 'i') })).toBeVisible();

    // Complete the reset out-of-band (token delivered via the fixture mailbox).
    const token = await consumeResetToken(USER);
    await page.goto(`/reset?token=${token}`);
    await page.getByLabel(/new password/i).fill(NEW_PASSWORD);
    await page.getByLabel(/confirm/i).fill(NEW_PASSWORD);
    await page.getByRole('button', { name: /set|save|update|reset/i }).click();

    // The previously-authenticated session cookie must no longer be honoured:
    // navigating an authed route bounces to /login (old HttpOnly cookie -> 401).
    await context.pages()[0].goto('/');
    await expect(page).toHaveURL(/\/login/);

    // The old password no longer authenticates.
    await login(page, USER, OLD_PASSWORD);
    await expect(page.getByText(/invalid|incorrect|try again/i)).toBeVisible();
    await expect(page).toHaveURL(/\/login/);
  });

  test('next login (A1) succeeds with the new password as the same principal', async ({ page }) => {
    const token = await consumeResetToken(USER);
    await page.goto(`/reset?token=${token}`);
    await page.getByLabel(/new password/i).fill(NEW_PASSWORD);
    await page.getByLabel(/confirm/i).fill(NEW_PASSWORD);
    await page.getByRole('button', { name: /set|save|update|reset/i }).click();

    await login(page, USER, NEW_PASSWORD);
    // Same principal identity is surfaced post-reset (A1 continuity).
    await expect(page.getByRole('button', { name: new RegExp(USER, 'i') })).toBeVisible();
    await expect(page).not.toHaveURL(/\/login/);
  });
});

// login drives the A1 leg with semantic locators.
async function login(page: import('@playwright/test').Page, username: string, password: string) {
  await page.goto('/login');
  await page.getByLabel(/username/i).fill(username);
  await page.getByLabel(/password/i).fill(password);
  await page.getByRole('button', { name: /log ?in|sign ?in/i }).click();
}

// consumeResetToken retrieves the single-use token the fixture delivered out-of-band.
// Epic 15 binds this to the throwaway auth-schema fixture's test mailbox endpoint;
// the token never crosses the reset-request HTTP body (see the Go contract note).
async function consumeResetToken(_username: string): Promise<string> {
  throw new Error(
    'consumeResetToken: bind to the Epic 15 auth-schema fixture mailbox when console source lands (ISI-2311).',
  );
}
