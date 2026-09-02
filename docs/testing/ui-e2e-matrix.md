# UI E2E Coverage Matrix — K8squad Console (live env)

**Issue:** ISI-3535 (parent ISI-3534) · **Owner:** Architect (Winston) · **Inputs:** Testing Architect, Observability Agent
**Target under test:** `http://10.0.0.219/` — entry `http://10.0.0.219/login?next=%2F`
**Status:** source of truth for the Playwright suite (ISI-3536). Update this doc, not the suite, when coverage changes.

---

## 0. How to read this matrix

Every row is one independently-runnable Playwright scenario. Columns:

| Column | Meaning |
|--------|---------|
| **id** | Stable scenario id (`AREA-NN`). Playwright test titles MUST embed this id so failures map back here 1:1. |
| **area** | `gateway` / `auth` / `routing` / `nav` / `dashboard` / `agents` / `projects` / `runs` / `compose` / `credentials` / `settings` / `audit` / `users` / `rbac` / `resilience`. |
| **precondition** | State that must hold before steps run (session, seed data, viewport). |
| **steps** | Ordered user actions. |
| **expected** | The assertion(s) that make the row PASS. |
| **svc** | Server-side services the scenario exercises, so Observability can point enrichment at the right pod + trace when a row fails (ISI-3539/3536). Codes below. |
| **prio** | P0 = board pain point / smoke-blocker · P1 = core surface · P2 = polish / edge. |

### `svc` legend — services exercised (for Observability targeting)

Requested by Observability (ISI-3539): each scenario names which server-side services it should light up. Console + apiserver both emit OTLP traces to the OTel gateway → Dynatrace (`oat05854`), so any UI action that reaches the backend is already a distributed trace — this column says which spans to expect.

| code | service | pod / target (ns `k8squad-system`) |
|------|---------|-------------------------------------|
| `GW` | kgateway (gateway VIP `10.0.0.219`) | kgateway data-plane; a `GW`-only failure = routing/gateway, not app. |
| `BFF` | Console BFF (Next.js server: route handlers, server components, `safeNext()`/session guard) | `console` deployment. |
| `API` | apiserver (`/auth/*`, `/api/*` data, `requireAdmin`/write-tier gates) | `apiserver` deployment; emits OTLP. |
| `OP` | operator (CRD reconcile — only rows that create/mutate CRDs) | operator deployment; watch its logs when an `OP` row fails post-apply. |

Read the column as the **expected** span fan-out: a request-path row is typically `GW→BFF→API`; a static/guard-only row may stop at `GW→BFF`; only CRD-mutating rows reach `OP`. A trace missing a hop it should have is itself the signal (e.g. an `API` row whose trace has no apiserver span → BFF never proxied).

### Environment & fixtures (parameterized — no secrets committed)

The suite (ISI-3536) takes these as params/env, defaulted for the live box:

| Param | Default | Notes |
|-------|---------|-------|
| `BASE_URL` | `http://10.0.0.219` | Gateway VIP (kgateway, ISI-3486/3515). |
| `ADMIN_USER` | `admin` | Bootstrap admin (ISI-3530). |
| `ADMIN_PASS` | *(env `E2E_ADMIN_PASS`)* | Never committed. Suite fails fast with a clear message if unset. |
| `VIEWER_USER` / `VIEWER_PASS` | *(optional)* | Enables the `rbac` rows; those rows `skip()` with reason if unset. |

### Auth contract (verified against source)

- Login is **username + password**. Form POSTs same-origin JSON `{username,password}` to BFF `/api/session`, which proxies apiserver `POST /auth/login` and sets an **opaque HttpOnly `ksquad_session` cookie** (`console/app/login/page.tsx`, `internal/apiserver/authroutes.go`).
- On success the client hard-navigates to the `?next=` target; `next` is open-redirect-guarded to **root-relative paths only** (`safeNext()` — rejects `//host` and any scheme).
- Error mapping: `401 → "Invalid username or password."`, `429 → too many attempts`, `400 → "Enter both…"`, other → generic. The suite asserts on these exact human strings for the negative rows.
- Nav is **role-adapted server-side** (`app/(app)/layout.tsx` → `viewerAccess()`): admin-only nodes (`Users & Roles`) are **absent from the DOM** for non-admins, not hidden — authz, not CSS.

### Suggested Playwright storage-state strategy
One-time `admin.setup.ts` performs AUTH-03 and saves `storageState` (the `ksquad_session` cookie); authenticated rows reuse it. `viewer.setup.ts` mirrors it when viewer creds are provided. This keeps the login flow itself covered explicitly (AUTH rows) while the rest of the suite runs pre-authenticated and fast.

---

## 1. Gateway reachability — board pain point (1)

| id | area | precondition | steps | expected | svc | prio |
|----|------|--------------|-------|----------|-----|------|
| GW-01 | gateway | none | GET `BASE_URL/` (no cookie) | Response received (not connection-refused/timeout); HTTP status is a real HTTP response (200/302/3xx/401), TLS/socket does not error. | GW→BFF | P0 |
| GW-02 | gateway | none | GET `BASE_URL/login` | 200; `content-type: text/html`; body non-empty. | GW→BFF | P0 |
| GW-03 | gateway | none | GET `BASE_URL/api/session` (no cookie) | Reaches the BFF (JSON or 401/403 — not a 502/504 gateway error). Proves BFF is routed behind the gateway. | GW→BFF→API | P0 |
| GW-04 | gateway | none | GET a static asset referenced by `/login` (e.g. `/_next/...` chunk or the Logo SVG) | 200 with correct content-type; asset actually loads (no broken chunk → confirms Next static routing through the gateway). | GW→BFF | P1 |

## 2. Login page renders — board pain point (2)

| id | area | precondition | steps | expected | svc | prio |
|----|------|--------------|-------|----------|-----|------|
| LOGIN-01 | auth | none | Navigate `BASE_URL/login?next=%2F` | Page renders the sign-in card: heading "Sign in", **Username** field, **Password** field, submit button, K8squad wordmark/Logo. No console errors, no ConsoleShell/nav rail present (shell-free). | GW→BFF | P0 |
| LOGIN-02 | auth | none | Inspect brand panel | Hero copy "Operate your agent squads with confidence." and the three feature bullets render on wide viewport. | GW→BFF | P2 |
| LOGIN-03 | auth | none | Submit with both fields empty | Client stays on page; apiserver/BFF `400` maps to inline error "Enter both your username and password." (`role="alert"`). No navigation. | GW→BFF | P1 |
| LOGIN-04 | auth | none | Enter bad username/password, submit | Inline error "Invalid username or password." (`401`); still on `/login`; no `ksquad_session` cookie set. | GW→BFF→API | P0 |
| LOGIN-05 | auth | mobile viewport (375px) | Load `/login` | Form panel leads (brand panel `aria-hidden`/hidden); fields usable; no horizontal scroll. | GW→BFF | P2 |

## 3. Default admin account works — board pain point (3)

| id | area | precondition | steps | expected | svc | prio |
|----|------|--------------|-------|----------|-----|------|
| ADMIN-01 | auth | valid `ADMIN_USER`/`ADMIN_PASS` | On `/login`, enter admin creds, submit | Request to `/api/session` returns `2xx`; `ksquad_session` HttpOnly cookie is set on the `BASE_URL` origin. | GW→BFF→API | P0 |
| ADMIN-02 | auth | ADMIN-01 done | After submit | Client hard-navigates off `/login` to `/` (the `next` default); authenticated shell renders. | GW→BFF→API | P0 |
| ADMIN-03 | auth | authenticated (admin storageState) | Load `/` | Admin-adapted nav present: **Users & Roles** node IS in the DOM (admin-only node visible). | GW→BFF→API | P0 |
| ADMIN-04 | auth | authenticated | GET `/api/session` (or `/api/me`) | Returns the admin identity (username `admin`, admin access tier). | GW→BFF→API | P1 |

## 4. Login → app routing / `?next` redirect — board pain point (4)

| id | area | precondition | steps | expected | svc | prio |
|----|------|--------------|-------|----------|-----|------|
| ROUTE-01 | routing | no cookie | Navigate to a protected page e.g. `BASE_URL/agents` | Redirected to `/login?next=%2Fagents` (unauth guard sends you to login preserving intended dest). | GW→BFF | P0 |
| ROUTE-02 | routing | no cookie | Complete login from ROUTE-01 with admin creds | After auth, land on `/agents` (the original `next` target), not `/`. | GW→BFF→API | P0 |
| ROUTE-03 | routing | no cookie | Navigate `BASE_URL/login?next=%2F` and log in | Land on `/` (root dashboard). | GW→BFF→API | P0 |
| ROUTE-04 | routing | no cookie | Navigate `BASE_URL/login?next=https://evil.example/` and log in | Open-redirect guard fires: land on `/` (same origin), NOT the external URL. | GW→BFF→API | P0 |
| ROUTE-05 | routing | no cookie | Navigate `BASE_URL/login?next=%2F%2Fevil.example` and log in | Guard rejects protocol-relative `//host`; land on `/`. | GW→BFF→API | P1 |
| ROUTE-06 | routing | authenticated | Navigate directly to `/login` while already logged in | Either redirected into the app or login renders without breaking session (document actual behavior; assert no error state / no session drop). | GW→BFF | P2 |
| ROUTE-07 | routing | authenticated | Hit an unknown path e.g. `/does-not-exist` | Branded not-found renders (root `not-found.tsx`) WITHOUT leaking the operator nav shell for the unauth case; for auth case document shell presence. | GW→BFF | P2 |

## 5. Navigation shell — every nav node reachable

Nav tree source: `console/lib/nav.ts`. Global nodes: Dashboard `/`, Overview `/overview`, Project `/projects/:id` (children Build/Tickets/Runs/Discussion), Agents `/agents`, Compose `/compose`, Users & Roles `/users` (admin-only), Settings `/settings` (child Configuration `/settings/configuration`). Audit surface at `/audit`.

| id | area | precondition | steps | expected | svc | prio |
|----|------|--------------|-------|----------|-----|------|
| NAV-01 | nav | authenticated (admin) | Load `/`; read left rail | Rail renders with home link + all admin-visible primary nodes (Dashboard, Overview, Project, Agents, Compose, Users & Roles, Settings). | GW→BFF→API | P0 |
| NAV-02 | nav | authenticated | Click each primary nav node in turn | Each navigates to its href, target page renders (no 404/error boundary), active state highlights the clicked node. | GW→BFF→API | P0 |
| NAV-03 | nav | authenticated | Expand Settings; click Configuration child | Navigates to `/settings/configuration`; page renders. | GW→BFF→API | P1 |
| NAV-04 | nav | authenticated, a project selected | Expand Project; click Build/Tickets/Runs/Discussion | Each project sub-route (`/projects/:id/{build,tickets,runs,discussion}`) renders. | GW→BFF→API | P1 |
| NAV-05 | nav | authenticated | Read breadcrumb on a nested page (e.g. project runs) | Breadcrumb reflects hierarchy (Dashboard → Projects → :id → section) with correct links. | GW→BFF | P2 |
| NAV-06 | nav | authenticated, tablet viewport (768–1024) | Load app | Rail is icon-only/collapsible; tapping expand shows labels. | GW→BFF | P2 |
| NAV-07 | nav | authenticated, mobile viewport (<768) | Load app | Bottom nav + drawer present; opening drawer lists nav nodes; closing works. | GW→BFF | P2 |
| NAV-08 | nav | authenticated | Use ProjectSelector to switch project | Project-scoped nav hrefs rebind to the chosen project id; navigating stays within that project. | GW→BFF→API | P1 |

## 6. Core Console pages — render + primary content

| id | area | precondition | steps | expected | svc | prio |
|----|------|--------------|-------|----------|-----|------|
| DASH-01 | dashboard | authenticated | Load `/` | Dashboard renders its primary content (no error boundary, no infinite spinner); data loads from BFF. | GW→BFF→API | P0 |
| OVIEW-01 | dashboard | authenticated | Load `/overview` | Overview/squad-overview renders; fetch to `/api/squad/overview` succeeds; no error state. | GW→BFF→API | P0 |
| AGENT-01 | agents | authenticated | Load `/agents` | Agents list renders; `/api/agents` returns; rows/empty-state shown (not error). | GW→BFF→API | P0 |
| AGENT-02 | agents | ≥1 agent exists | Click an agent row | Navigates to `/agents/:agentId`; detail renders (agent identity + runs). | GW→BFF→API | P1 |
| AGENT-03 | agents | agent detail loaded | View agent runs section | `/api/agents/:id/runs` resolves; runs list or empty-state shown. | GW→BFF→API | P2 |
| PROJ-01 | projects | authenticated | Navigate to a project `/projects/:id` | Project root renders; project data loads. | GW→BFF→API | P0 |
| PROJ-02 | projects | project loaded | Open Tickets `/projects/:id/tickets` | Ticket/work-item list renders; `/api/work-items` (or project tickets) resolves. | GW→BFF→API | P1 |
| PROJ-03 | projects | project loaded | Open Build `/projects/:id/build` | Build surface renders. | GW→BFF→API | P1 |
| PROJ-04 | projects | project loaded | Open Discussion `/projects/:id/discussion` | Discussion room renders; RoomClient mounts (stream/rooms endpoint reachable). | GW→BFF→API | P2 |
| PROJ-05 | projects | project loaded | Open project Runs `/projects/:id/runs` | Runs list for the project renders. | GW→BFF→API | P1 |
| RUN-01 | runs | ≥1 run exists | From a runs list, open a run `/runs/:runId` | Run detail renders (phase/status); `/api/runs/:id` resolves. | GW→BFF→API | P0 |
| RUN-02 | runs | run detail loaded | View run build view `/runs/:runId/build` | Build view renders. | GW→BFF→API | P2 |
| RUN-03 | runs | run detail loaded | View artifacts `/runs/:runId/artifacts` | Artifacts list renders; `/api/runs/:id/artifacts` resolves (list or empty-state). | GW→BFF→API | P1 |
| RUN-04 | runs | a live/streaming run available | Open run detail | Live log stream connects (`/api/runs/:id/stream` or `/logs`); log lines appear or a clean "no logs yet" state (no crash). | GW→BFF→API (+OP: run producer) | P2 |
| COMPOSE-01 | compose | authenticated | Load `/compose` | CRD authoring surface renders (Team/Project/Agent/Role/Skill selectors). | GW→BFF→API | P1 |
| COMPOSE-02 | compose | admin | Author + submit a valid CRD (e.g. a throwaway Skill/Agent) | Submit hits `/api/compose/:kind`; success feedback; the object appears where listed. *(Mutating — gate behind a `--allow-writes` flag; default P2/skip.)* | GW→BFF→API→OP | P2 |
| CRED-01 | credentials | authenticated | Load `/credentials` | Credentials page renders; `/api/credentials` resolves; list or empty-state (no error). | GW→BFF→API | P1 |
| CRED-02 | credentials | credentials page loaded | Inspect connect affordance | The connect/`/api/credentials/connect` entry point is present and not a dead control. | GW→BFF→API | P2 |
| SET-01 | settings | authenticated | Load `/settings` | Settings renders. | GW→BFF→API | P1 |
| SET-02 | settings | authenticated | Load `/settings/configuration` | Configuration (OTLP exporter surface) renders; current config loads. | GW→BFF→API | P1 |
| AUDIT-01 | audit | admin | Load `/audit` | Audit log page renders; `/api/audit` resolves; entries or empty-state (no error). | GW→BFF→API | P1 |
| USERS-01 | users | admin | Load `/users` | Users & Roles admin surface renders; `/api/admin/users` resolves; admin user listed. | GW→BFF→API | P0 |
| USERS-02 | users | admin | Inspect a user's role controls | Role assignment controls render (read-only assertion; mutation gated behind `--allow-writes`). | GW→BFF→API | P2 |

## 7. RBAC / role-adapted surface (runs only when viewer creds provided)

| id | area | precondition | steps | expected | svc | prio |
|----|------|--------------|-------|----------|-----|------|
| RBAC-01 | rbac | authenticated as **viewer** | Load `/`; read nav | **Users & Roles** node is ABSENT from the DOM (admin-only node removed server-side). | GW→BFF→API | P1 |
| RBAC-02 | rbac | viewer | Navigate directly to `/users` | Access denied / redirect — not the admin surface (apiserver `requireAdmin` is the wall; assert 403/guard, no user data leaked). | GW→BFF→API | P1 |
| RBAC-03 | rbac | viewer | Navigate directly to `/audit` | Non-admin is blocked or shown an authorized-only state (document + assert actual behavior). | GW→BFF→API | P2 |
| RBAC-04 | rbac | viewer | Attempt a write via Compose | apiserver write-tier gate returns 403; UI surfaces the denial without crashing. | GW→BFF→API (403 at API; no OP) | P2 |

## 8. Resilience / cross-cutting

| id | area | precondition | steps | expected | svc | prio |
|----|------|--------------|-------|----------|-----|------|
| RES-01 | resilience | authenticated | Full-page reload on a deep route (e.g. `/projects/:id/runs`) | Route rehydrates correctly (SSR/route-group boundary intact); no shell leak, no hydration error in console. | GW→BFF→API | P1 |
| RES-02 | resilience | authenticated | Delete/expire `ksquad_session` cookie, then navigate to a protected page | Redirected to `/login?next=…` (session guard); no stack trace / raw error page. | GW→BFF | P1 |
| RES-03 | resilience | authenticated | Walk every P0/P1 page and collect browser console output | No uncaught exceptions / no failed critical BFF requests (4xx/5xx) beyond expected auth negatives. Feed into Observability capture (ISI-3539). | GW→BFF→API (all hops on the walk) | P1 |
| RES-04 | resilience | any | Load `/login` and each core page; capture failed network requests | No 5xx from the gateway/BFF on happy paths; any 5xx is a bug row for triage (ISI-3538). | GW→BFF→API (all hops on the walk) | P1 |

---

## 9. Priority rollup (suite ordering)

- **P0 (smoke — must pass before anything else counts):** GW-01..03, LOGIN-01/04, ADMIN-01..03, ROUTE-01..04, NAV-01/02, DASH-01, OVIEW-01, AGENT-01, PROJ-01, RUN-01, USERS-01. These ARE the board's four pain points plus the core-page smoke.
- **P1:** remaining core-page renders, project sub-nav, RBAC visibility, resilience.
- **P2:** responsive breakpoints, deep-detail/streaming, mutating write flows (gated behind `--allow-writes`).

## 10. Handoff notes

- **Testing Architect (ISI-3536):** implement one Playwright test per row; embed the `id` in each test title. P0 rows are the required happy-path set; mutating rows (COMPOSE-02, USERS-02, RBAC-04) must be behind an opt-in `--allow-writes` flag and default-skipped so the suite is safe to run repeatedly against the live box. Suite is **not** a required CI gate (per ISI-3534) — it's a bug-surfacing harness parameterized by `BASE_URL`/creds.
- **Observability (ISI-3539):** RES-03/RES-04 are the failure-capture hooks — console + network capture per row, plus per-failure cluster logs/traces. Shape the capture contract against these rows now. The **`svc` column** (per your ask) tells you which service spans each row should light up, so on a failure you point enrichment straight at the named pod (`console`/`apiserver`/kgateway/operator) + its OTLP trace instead of grepping all of `k8squad-system`. A trace missing a hop the row's `svc` promises is itself a triage signal.
- **ProxOps (ISI-3537):** run order = P0 → P1 → P2; publish per-`id` PASS/FAIL + trace/screenshot artifacts.
- Scope guard: this matrix covers the **live Console UI surface** enumerated from `console/app/(app)/**` and `console/lib/nav.ts` as of 2026-09-02. New pages/nav nodes → add a row here first.
