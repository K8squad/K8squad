# UI E2E Coverage Matrix — K8squad Console (live env)

**Issue:** ISI-3535 (parent ISI-3534) · **Owner:** Architect (Winston) · **Inputs:** Testing Architect, Observability Agent
**Target under test:** `http://10.0.0.219/` — entry `http://10.0.0.219/login?next=%2F`
**Status:** source of truth for the Playwright suite (ISI-3536). Update this doc, not the suite, when coverage changes.

> **Scope (board refinement, ISI-3534, 2026-09-02):** _"limit to the login page, please build test covering most of our UI features."_
> The **login page is the required, in-scope surface** and is covered **exhaustively** below (§1–§8). It is the reliably-reachable surface today because login→app routing and the default-admin path are still suspect — so we go deep on what `/login` exposes rather than shallow across many pages.
> **Authenticated Console pages are DEFERRED** (not deleted) — see §10. Their rows stay enumerated so we lose nothing; they move into required scope once the auth/routing path is verified working. The valid-credentials happy-path rows (§6) **stay in the required suite with their assertions intact** — if routing is broken they will FAIL, and that failure is exactly the bug we want to capture.

---

## 0. How to read this matrix

Every row is one independently-runnable Playwright scenario. Columns:

| Column | Meaning |
|--------|---------|
| **id** | Stable scenario id (`AREA-NN`). Playwright test titles MUST embed this id so failures map back here 1:1. |
| **area** | `gateway` / `render` / `form` / `behavior` / `auth` / `routing` / `a11y` / `responsive` / `resilience` / `security`. |
| **precondition** | State that must hold before steps run (viewport, cookie state). |
| **steps** | Ordered user actions. |
| **expected** | The assertion(s) that make the row PASS. |
| **svc** | Expected server-side span fan-out (Observability, ISI-3539). `GW`=kgateway (VIP 10.0.0.219) · `BFF`=Console (`console` deploy) · `API`=apiserver (`apiserver` deploy, OTLP) · `OP`=operator. A trace missing a hop its `svc` promises is itself a triage signal. |
| **prio** | P0 = board pain point / smoke-blocker · P1 = core login behavior · P2 = polish / edge. |

### Environment & fixtures (parameterized — no secrets committed)

| Param | Default | Notes |
|-------|---------|-------|
| `BASE_URL` | `http://10.0.0.219` | Gateway VIP (kgateway, ISI-3486/3515). |
| `ADMIN_USER` | `admin` | Bootstrap admin (ISI-3530). Used only by the valid-cred rows (§6). |
| `ADMIN_PASS` | *(env `E2E_ADMIN_PASS`)* | Never committed. Suite `skip()`s the valid-cred rows with a clear reason if unset. |

### Login page — verified structure & contract (source of truth)

Verified against `console/app/login/page.tsx`, `console/app/api/session/route.ts`, `internal/apiserver/authroutes.go`:

- **Two panels.** A **brand panel** (`aria-hidden="true"`, wide-viewport only): `<Logo>` mark, eyebrow "K8squad Console", hero "Operate your agent squads with confidence.", a lede, and three bullets ("Live run streams & provenance", "CRD authoring & composition", "Role-scoped, audited access"). A **form panel** always present.
- **Form card:** `<Logo>` + bold wordmark "K8squad"; `<h2>` "Sign in"; subtitle "Welcome back. Sign in to reach your console."; the sign-in `<form noValidate>`; an informational SSO line "Using single sign-on? Contact your workspace administrator." (**plain text — NOT a link**); legal line "K8squad · Kubernetes-native agent squads".
- **Fields:** Username `<input type=text name=username autoComplete=username autoCapitalize=none autoCorrect=off spellCheck=false required autoFocus>`; Password `<input type=password name=password autoComplete=current-password required>`. Both `disabled` while submitting.
- **Submit button:** `type=submit`, `disabled` when `submitting || !username || !password`. Label toggles "Sign in" → "Signing in…" while in flight. **Empty-field validation is expressed by the disabled button, not a server 400** — the form is `noValidate`, so there are no native browser bubbles.
- **Submit flow:** POST same-origin JSON `{username,password}` → BFF `/api/session` → apiserver `POST /auth/login` → sets **opaque HttpOnly `ksquad_session` cookie**. On `res.ok`, client hard-navigates via `window.location.assign(safeNext())`.
- **`next` handling — `safeNext()`:** returns the `?next=` value only if it starts with `/` and not `//`; otherwise `/`. Open-redirect guard: `//host`, `http(s)://…`, or any scheme → `/`.
- **Error copy mapping (exact strings the suite asserts on):** `401 → "Invalid username or password."` · `429 → "Too many attempts. Please wait a moment and try again."` · `400 → "Enter both your username and password."` · other status → `"Sign-in is temporarily unavailable. Please try again."` · network/throw → `"Can’t reach the sign-in service. Check your connection and retry."` Errors render in `<p role="alert" aria-live="assertive">`.

---

## 1. Gateway reachability — board pain point (1)

| id | area | precondition | steps | expected | svc | prio |
|----|------|--------------|-------|----------|-----|------|
| GW-01 | gateway | none | GET `BASE_URL/login?next=%2F` (no cookie) | A real HTTP response arrives (not connection-refused/timeout/TLS error); status is 200. | GW→BFF | P0 |
| GW-02 | gateway | none | Inspect GW-01 response headers/body | `content-type: text/html`; body non-empty; not a 502/504 gateway error page. | GW→BFF | P0 |
| GW-03 | gateway | none | GET the `/login` static assets (Next `/_next/...` chunk + Logo SVG) referenced by the page | Each returns 200 with correct content-type — page JS/CSS actually loads through the gateway (no broken chunk). | GW→BFF | P1 |
| GW-04 | gateway | none | OPTIONS/GET `BASE_URL/api/session` (no cookie) | Reaches the BFF (JSON or 401/403 — not 502/504). Proves the login form's POST target is routed. | GW→BFF | P0 |

## 2. Page renders + branding — board pain point (2)

| id | area | precondition | steps | expected | svc | prio |
|----|------|--------------|-------|----------|-----|------|
| LOGIN-01 | render | none | Navigate `BASE_URL/login?next=%2F` | Sign-in card renders: `<h2>` "Sign in", Username field, Password field, submit button. **No** ConsoleShell/nav rail present (shell-free layout). | GW→BFF | P0 |
| LOGIN-02 | render | none | Locate branding | K8squad `<Logo>` mark AND the bold "K8squad" wordmark render in the form card; no broken image. | GW→BFF | P0 |
| LOGIN-03 | render | wide viewport (≥1024px) | Inspect brand panel | Hero "Operate your agent squads with confidence.", lede, and all three feature bullets render; panel is `aria-hidden`. | GW→BFF | P2 |
| LOGIN-04 | render | none | Inspect supporting copy | Subtitle "Welcome back…", SSO note "Using single sign-on? Contact your workspace administrator.", and legal line "K8squad · Kubernetes-native agent squads" all present. | GW→BFF | P2 |

## 3. Login form structure

| id | area | precondition | steps | expected | svc | prio |
|----|------|--------------|-------|----------|-----|------|
| FORM-01 | form | `/login` loaded | Inspect Username input | `type=text`, `name=username`, `autocomplete=username`, `autocapitalize=none`, `autocorrect=off`, `spellcheck=false`, `required`, and it is `autofocus`ed on load. | GW→BFF | P1 |
| FORM-02 | form | `/login` loaded | Inspect Password input | `type=password`, `name=password`, `autocomplete=current-password`, `required`; value is masked. | GW→BFF | P1 |
| FORM-03 | form | `/login` loaded | Inspect labels | Both fields have visible text labels ("Username","Password") associated via wrapping `<label>`. | GW→BFF | P1 |
| FORM-04 | form | `/login` loaded | Inspect submit button | Present, `type=submit`, label reads "Sign in". | GW→BFF | P1 |
| FORM-05 | form | `/login` loaded | Enumerate anchors/links in the form card | **No** "forgot/reset password" link and **no** SSO link exist (SSO is plain text). Assertion documents this so a future dead link is caught; there are therefore no secondary links to 200/404-check. | GW→BFF | P1 |

## 4. Form behavior & client validation

| id | area | precondition | steps | expected | svc | prio |
|----|------|--------------|-------|----------|-----|------|
| BEH-01 | behavior | `/login` loaded | Observe submit button with both fields empty | Button is `disabled` (empty-field guard) — cannot submit; no network request fires. | GW→BFF | P0 |
| BEH-02 | behavior | `/login` loaded | Type into username only, leave password empty | Button stays `disabled` until BOTH fields are non-empty. | GW→BFF | P1 |
| BEH-03 | behavior | `/login` loaded | Fill both fields | Button becomes enabled. | GW→BFF | P1 |
| BEH-04 | behavior | both fields filled | Click submit; observe in-flight state | Button label → "Signing in…"; button and both inputs become `disabled` for the request duration. | GW→BFF→API | P1 |
| BEH-05 | behavior | both fields filled | Submit and observe request | Exactly one POST to `/api/session` with JSON `{username,password}` (content-type application/json). | GW→BFF→API | P1 |

## 5. Invalid / error states — no crash

| id | area | precondition | steps | expected | svc | prio |
|----|------|--------------|-------|----------|-----|------|
| ERR-01 | auth | `/login` loaded | Enter a known-bad username/password, submit | Inline `role="alert"` error "Invalid username or password." (401 mapping); still on `/login`; **no** `ksquad_session` cookie set; page does not crash. | GW→BFF→API | P0 |
| ERR-02 | auth | `/login` loaded | Rapidly submit bad creds enough to trip rate-limit (if enforced) | If a 429 occurs, error reads "Too many attempts. Please wait a moment and try again." *(Conditional — assert only when a 429 is observed; do not hammer the live box: cap attempts.)* | GW→BFF→API | P2 |
| ERR-03 | auth | `/login` loaded | Simulate an unreachable session service (offline/route block in test context) | Network-failure branch shows "Can’t reach the sign-in service. Check your connection and retry."; button re-enables. | GW→BFF | P2 |
| ERR-04 | auth | after any error | Inspect error element | Error is in `<p role="alert" aria-live="assertive">` so it is announced to assistive tech. | GW→BFF | P1 |

## 6. Valid credentials happy path + `?next` routing — board pain points (3) & (4)

> These rows exercise default-admin (3) and login→app routing (4). Per the board, **keep the assertions even though they may FAIL today** — a failure here is the tracked bug for ISI-3538, not a reason to weaken the row. `skip()` (with reason) only if `ADMIN_PASS` is unset.

| id | area | precondition | steps | expected | svc | prio |
|----|------|--------------|-------|----------|-----|------|
| OK-01 | auth | valid `ADMIN_USER`/`ADMIN_PASS` | Enter admin creds on `/login?next=%2F`, submit | `/api/session` returns 2xx; `ksquad_session` HttpOnly cookie set on `BASE_URL` origin. **(May fail if default admin broken — capture as bug.)** | GW→BFF→API | P0 |
| OK-02 | routing | OK-01 succeeded | Observe post-submit navigation | Client hard-navigates OFF `/login` to the `next` target `/`. **(May fail if routing broken — capture as bug.)** | GW→BFF→API | P0 |
| OK-03 | routing | valid creds | Load `/login?next=%2Fagents`, submit | After auth, land on `/agents` (the `next` target preserved through submit). **(May fail — capture as bug.)** | GW→BFF→API | P0 |
| OK-04 | auth | no cookie | Navigate directly to a protected route e.g. `/agents` | Redirected to `/login?next=%2Fagents` (unauth guard preserves intended dest). | GW→BFF | P1 |

## 7. `next` open-redirect guard — security

| id | area | precondition | steps | expected | svc | prio |
|----|------|--------------|-------|----------|-----|------|
| SEC-01 | security | valid creds | Load `/login?next=%2F%2Fevil.example`, submit | `safeNext()` rejects protocol-relative `//host`; navigation goes to `/`, never `evil.example`. | GW→BFF→API | P0 |
| SEC-02 | security | valid creds | Load `/login?next=https%3A%2F%2Fevil.example%2F`, submit | Scheme URL rejected; land on `/`. | GW→BFF→API | P0 |
| SEC-03 | security | none | Load `/login?next=%2Fsettings%2Fconfiguration` | The `next` value round-trips into the form's redirect intent (root-relative path accepted). | GW→BFF | P1 |

## 8. Accessibility, responsive & resilience

| id | area | precondition | steps | expected | svc | prio |
|----|------|--------------|-------|----------|-----|------|
| A11Y-01 | a11y | `/login` loaded | Check initial focus | Username input is focused on load (`autoFocus`). | GW→BFF | P1 |
| A11Y-02 | a11y | `/login` loaded | Tab through the page | Focus order is logical: Username → Password → Submit; no keyboard trap. | GW→BFF | P1 |
| A11Y-03 | a11y | both fields filled | Press Enter inside a field | Form submits (native `<form onSubmit>`) — same path as clicking Submit. | GW→BFF→API | P1 |
| A11Y-04 | a11y | `/login` loaded | Run an axe/labelled-inputs check | Inputs are programmatically labelled; error region uses `role=alert`; no critical a11y violations. | GW→BFF | P2 |
| RESP-01 | responsive | mobile viewport (375×812) | Load `/login` | Form panel leads; brand panel hidden/`aria-hidden`; fields usable; no horizontal scroll. | GW→BFF | P1 |
| RESP-02 | responsive | desktop viewport (1440×900) | Load `/login` | Two-panel layout: brand hero left, form card right; no overlap/clipping. | GW→BFF | P2 |
| RES-01 | resilience | none | Load `/login`; collect browser console output | No uncaught exceptions and no console errors on load. Feeds Observability capture (ISI-3539). | GW→BFF | P1 |
| RES-02 | resilience | none | Load `/login`; capture all network requests | No failed (4xx/5xx) requests on initial render; any 5xx from GW/BFF is a bug row for triage (ISI-3538). | GW→BFF | P1 |
| RES-03 | resilience | none | Full reload of `/login?next=%2F` | Page rehydrates cleanly; no hydration error in console; shell-free layout intact. | GW→BFF | P2 |

---

## 9. Priority rollup (required suite ordering)

- **P0 (must pass / board pain points):** GW-01/02/04, LOGIN-01/02, BEH-01, ERR-01, OK-01..03, SEC-01/02. These are gateway-reach + login-render + default-admin + `?next` routing + open-redirect. OK-01..03 may FAIL today by design (captured as bugs).
- **P1:** form structure, in-flight behavior, error a11y, unauth guard, `next` round-trip, responsive-mobile, console/network resilience.
- **P2:** brand-panel copy, rate-limit/offline branches, desktop layout, axe pass, hydration reload.

## 10. DEFERRED — authenticated Console pages (out of required scope until auth path works)

> Kept enumerated so we don't lose coverage. These move into the required suite once OK-01..03 pass (login→app routing + default-admin verified). Tracked under the parent ISI-3534; ProxOps (ISI-3537) should **not** be blocked waiting on these.

Deferred areas and representative rows (full behavioral detail to be re-expanded when unblocked):

- **nav shell** — rail/breadcrumb/project-selector reachability across Dashboard `/`, Overview `/overview`, Project `/projects/:id` (Build/Tickets/Runs/Discussion), Agents `/agents`, Compose `/compose`, Users & Roles `/users` (admin-only), Settings `/settings` → Configuration. `svc: GW→BFF→API`.
- **dashboard / overview** — `/` and `/overview` render + data load (`/api/squad/overview`). `svc: GW→BFF→API`.
- **agents** — `/agents` list, `/agents/:id` detail + runs. `svc: GW→BFF→API`.
- **projects** — `/projects/:id` root, `tickets`, `build`, `discussion` (RoomClient stream), `runs`. `svc: GW→BFF→API`.
- **runs** — `/runs/:id` detail, `build`, `artifacts`, live `stream`/`logs`. `svc: GW→BFF→API`.
- **compose** — `/compose` CRD authoring; write flow reaches the operator. `svc: GW→BFF→API→OP` (gate mutations behind `--allow-writes`).
- **credentials** — `/credentials` list + connect. `svc: GW→BFF→API`.
- **settings** — `/settings`, `/settings/configuration` (OTLP surface). `svc: GW→BFF→API`.
- **audit** — `/audit` admin log. `svc: GW→BFF→API`.
- **users** — `/users` admin identity surface. `svc: GW→BFF→API`.
- **rbac** — admin-only nodes absent from DOM for viewer; `requireAdmin` wall on `/users`,`/audit`; write-tier 403 on compose (stops at `API`, no `OP`). Needs a viewer credential.

## 11. Handoff notes

- **Testing Architect (ISI-3536):** implement one Playwright test per row in §1–§8; embed the row `id` in each test title. P0 set = the smoke/pain-point rows. Keep OK-01..03 **in the required suite with assertions intact** — a failing routing/admin path is the bug we want surfaced, not skipped. Mutating flows stay out of login scope entirely. Suite is parameterized by `BASE_URL`/creds and is **not** a required CI gate (per ISI-3534). The §10 deferred pages are explicitly NOT in this build.
- **Observability (ISI-3539):** the `svc` column names the expected span fan-out per row; RES-01/RES-02 are the console/network capture hooks. Login rows are `GW→BFF` (render/guard) or `GW→BFF→API` (submit) — no `OP` on the login page at all, so a login-path trace touching the operator is anomalous.
- **ProxOps (ISI-3537):** run order = P0 → P1 → P2 across §1–§8 only; publish per-`id` PASS/FAIL + trace/screenshot artifacts. Expect OK-01..03 to potentially fail — record, don't suppress.
- Scope guard: login-page coverage enumerated from `console/app/login/page.tsx` + `console/app/api/session/route.ts` as of 2026-09-02. Deferred pages enumerated from `console/app/(app)/**` + `console/lib/nav.ts`.
