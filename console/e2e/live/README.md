# Console UI E2E — live-environment suite

A parameterized [Playwright](https://playwright.dev) suite that exercises a **live, already-running
K8squad Console** and prints a **one-glance per-feature PASS / FAIL scoreboard**, so we can identify
UI/routing/auth bugs fast instead of taking baby steps.

> Parent: **ISI-3534** · Suite: **ISI-3536** · Coverage matrix: **ISI-3535** ·
> Runs/deploy: **ISI-3537** · Bug triage: **ISI-3538** · Logs/traces: **ISI-3539**

This lane is **NOT** a required GitHub CI Action — there is no ephemeral environment to test
against. It is a runnable suite (npm script) plus an **optional** `workflow_dispatch` job
(`.github/workflows/ui-e2e-live.yml`) that takes host/user/password inputs and **never gates PRs**.

## What it covers (the matrix rows)

| Row | Spec | Needs creds |
| --- | --- | --- |
| Gateway: root → `/login` redirect | `01-gateway.spec.ts` | no |
| Gateway: `/login` reachable (200) | `01-gateway.spec.ts` | no |
| Login page renders | `02-login.spec.ts` | no |
| Login: bad creds rejected (non-enumerating) | `02-login.spec.ts` | no |
| Default-admin login | `02-login.spec.ts` | **yes** |
| Login → app routing (authenticated shell) | `02-login.spec.ts` | **yes** |
| Each core Console page loads (Dashboard, Overview, Agents, Compose, Users & Roles, Settings, Settings → Configuration, Audit, Credentials) | `03-console-pages.spec.ts` | **yes** |
| Project pages (Build / Tickets / Runs / Discussion) | `03-console-pages.spec.ts` | **yes** (+ a project) |

Each page row asserts the page (a) responds `< 400`, (b) does **not** bounce back to `/login`
(auth/shell-leak guard), (c) renders the authenticated nav rail, and (d) shows no crash / server
error overlay.

Rows that need credentials **skip-with-reason** (never silently drop) when `KSQUAD_PASSWORD` is
unset — so the scoreboard always lists every feature.

## Parameters (nothing hardcoded except the requested default host)

| Env var | Default | Purpose |
| --- | --- | --- |
| `KSQUAD_BASE_URL` | `http://10.0.0.219` | base origin of the live Console |
| `KSQUAD_LOGIN_PATH` | `/login?next=%2F` | login route incl. `?next=` |
| `KSQUAD_USERNAME` | `admin` | admin username |
| `KSQUAD_PASSWORD` | _(none)_ | admin password — **required for authenticated rows; never committed** |
| `KSQUAD_PROJECT_ID` | _(auto-discover)_ | project id for project-scoped pages; auto-discovered from `/api/projects` if unset |
| `KSQUAD_E2E_RETRIES` | `1` | per-row retries (smooths transient network blips) |

## Run it

```bash
cd console
npm ci
npx playwright install --with-deps chromium   # first time only

# Full run against a live env (credentials via env — never commit them):
KSQUAD_BASE_URL=http://10.0.0.219 \
KSQUAD_USERNAME=admin \
KSQUAD_PASSWORD='********' \
  npm run e2e:live

# Unauthenticated smoke only (gateway + login render); authed rows skip-with-reason:
KSQUAD_BASE_URL=http://10.0.0.219 npm run e2e:live

# Open the deep HTML report (trace/screenshot/video per failing row):
npm run e2e:live:report
```

## Reports

| Artifact | Kind | Path |
| --- | --- | --- |
| Feature scoreboard | machine-readable | `test-results/live/feature-status.json` |
| Feature scoreboard | human-readable | `test-results/live/feature-status.md` |
| Full results | machine-readable | `test-results/live/results.json` |
| Deep report (trace/screenshot/video) | human-readable | `playwright-report/live/index.html` |

The scoreboard is also printed to stdout at the end of the run. Failing rows carry a Playwright
**trace**, a **screenshot**, a **video**, and an attached **console/network log** (coordinated with
the Observability Agent, ISI-3539) so ProxOps (ISI-3537) and the PM (ISI-3538) get enough context
to file and fix each bug.

## Convention

Semantic locators only (`getByRole` / `getByLabel` / `getByText`) — no CSS or test-id selectors,
matching the repo's E2E conventions. The page list is derived from the console nav model
(`lib/nav.ts`) so it cannot silently drift from the shipped navigation.
