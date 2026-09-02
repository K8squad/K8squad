# ADR-0003 — Console auth/login route + pre-auth layer (fix /login 404 + shell leak)

- **Status:** Accepted — *implemented*
- **Date:** 2026-09-01
- **Author:** Winston (System Architect)
- **Issue:** ISI-3522 (parent ISI-3520)
- **Coordinates with:** ISI-3523 (Graphic Designer — branded `/login` markup)
- **Scope:** Next.js App Router routing in `console/app` + one BFF handler. NO
  new authorization surface (the Go apiserver stays the ONE authz choke point,
  §13 / ADR-013).

## Context

The gateway (`http://10.0.0.219`) serves an **unauthenticated** visitor a bounce
to `/login?next=…`, but the console had **no `/login` route** and — worse — the
**root layout wrapped every page (including 404) in `<ConsoleShell>`**. So:

- `GET /` → `307 /login?next=%2F` (issued by the console's own `middleware.ts` /
  `lib/authGate.ts` — see the *Gateway contract* section; envoy does not do this).
- `GET /login` → **404**, and the 404 rendered the full operator nav rail with a
  `404: This page could not be found.` title. An anonymous visitor saw the
  operator shell instead of a sign-in screen — the "shell leak."

Root cause: `console/app/layout.tsx` mounted `<ConsoleShell>` at the **root**, so
it wrapped *everything* App Router rendered under it — real pages, `not-found`,
and any future pre-auth page alike.

## Decision

**1 — Route-group split.** The root layout becomes **bare** (html/body +
`ThemeProvider` only, no shell). The authenticated app moves under a
`(app)` **route group** whose own `layout.tsx` mounts `<ConsoleShell>` and
resolves the viewer's role (`viewerAccess()`). Route groups do **not** change
URLs, so `/`, `/overview`, `/agents`, `/audit`, … are unchanged.

```
console/app/
  layout.tsx            ← BARE: html/body + ThemeProvider (no shell)
  not-found.tsx         ← clean 404 on the bare layout (no shell leak)
  login/page.tsx        ← the /login route (branded, ISI-3523) — bare layout, shell-free
  (app)/
    layout.tsx          ← mounts ConsoleShell + viewerAccess() — wraps ONLY authed pages
    page.tsx  overview/  agents/  audit/  compose/  credentials/
    users/  settings/  projects/  runs/
  api/                  ← BFF route handlers (no layout applies)
```

Because `/login` and `not-found.tsx` sit **directly under the bare root layout**,
they render **outside** `ConsoleShell` with zero extra wrapping — no dedicated
`(auth)` group was needed. `<ConsoleShell>` now lives in exactly one place.

**2 — `not-found.tsx` at the app root.** A root `not-found.tsx` renders on the
bare layout, so stray/unknown routes (and any `notFound()` that bubbles past the
`(app)` group) get a clean page, not the shell-wrapped 404.

**3 — Auth flow (username + password, the live v1 method).**

```
/login form ──POST {username,password}──▶ /api/session (BFF, console)
                                             │ proxyAuth(): forwards body,
                                             │ RELAYS Set-Cookie
                                             ▼
                                  apiserver POST /auth/login  (§13 choke point)
                                             │ verifies argon2id creds, mints
                                             │ opaque session + Set-Cookie
                                             ▼
                              HttpOnly `ksquad_session` cookie lands in browser
/login ──window.location.assign(next)──▶ middleware re-runs WITH cookie ──▶ (app) shell
```

- The BFF (`console/app/api/session/route.ts`) gains `POST → /auth/login` and
  `DELETE → /auth/logout`. `GET` proxies `/auth/me` (role summary; ISI-3530).
- New BFF helper `proxyAuth()` (`console/lib/bff.ts`) is the **only** proxy that
  **relays upstream `Set-Cookie`** — the auth routes are the cookie's *issuer*.
  It surfaces status verbatim (401 opaque invalid-creds, 429 rate-limited) and
  handles null-body statuses (logout answers **204**) so the relay never 500s.
- The console **never** verifies credentials or mints the token — it forwards
  identity and relays the apiserver's decision. No second authz path (§13).
- `next` is sanitized to a same-origin absolute path (open-redirect guard) before
  navigation.
- **OIDC/SSO** is a config-only future leg (15.9 group-mapping seam already in
  `pkg/auth`); the login screen keeps it a non-blocking affordance, never a dead
  button.

## Gateway contract (what envoy actually does — and does not)

**The gateway does NOT validate the session.** The `deploy/helm/ksquad`
`Gateway` + `HTTPRoute` are dumb L7 routing: host `console.*` → console Service,
host `apiserver.*` → apiserver Service (with `timeout: 0s` to preserve SSE).
There is **no `ext_authz` filter** on the gateway.

The `307 → /login?next=` the PM observed is issued by the **console's own
middleware** (`console/middleware.ts` → `lib/authGate.ts::gateDecision`), which
runs in the Next.js server *behind* envoy — so the client sees it pass *through*
envoy, but envoy did not author it. The contract is therefore:

| Layer | Responsibility |
|-------|----------------|
| **Gateway (envoy)** | Pure host/path routing. No session inspection. |
| **Console middleware** | Presence-only gate: no `ksquad_session` cookie on a protected path → `307 /login?next=<dest>`. NEVER decodes/role-checks the cookie (a present-but-invalid cookie passes here and is rejected upstream). `/login`, `/api/*`, `/_next`, static are public. |
| **Apiserver (§13)** | The ONE authz boundary: verifies creds at `/auth/login`, mints/validates the opaque `ksquad_session`, applies the §12.3 deny-by-default RBAC wall. Fail-closed, existence-hiding. |

`next=` is honored entirely by the **app**: the middleware writes it when it
bounces an anonymous request; `/login` reads it and navigates there post-login
(sanitized). The gateway is never involved in the round-trip.

## Auth-endpoint hardening (Copilot review of PR #215)

The `POST /api/session` handler *sets the session cookie*, so it is a state-changing
endpoint that acts on the ambient cookie — i.e. a CSRF/login-CSRF sink and a
rate-limit relay. Three guards close that surface; all live in `console/lib/bff.ts`
so every auth mutation inherits them:

1. **Login-CSRF guard (`crossSiteReject`, applied in `proxyAuth`).** Before the
   apiserver is touched, the BFF fails closed on any of: `Sec-Fetch-Site` ∉
   {same-origin, same-site, none}; an `Origin` whose host ≠ the request host; or a
   declared `Content-Type` that is not `application/json`. This blocks the classic
   cross-origin `text/plain`/form-encoded "simple request" that carries valid JSON
   into a login and pins the victim to an attacker account. A rejected request never
   reaches `/auth/login` and never receives a `Set-Cookie` (403).
2. **Open-redirect guard (`sanitizeNext`, `app/login/page.tsx`).** The post-login
   `?next=` target must be root-relative AND resolve same-origin. In addition to
   rejecting `//host` and scheme URLs, it rejects the **backslash bypass**
   (`/\host` — browsers normalize `\`→`/`, so it is protocol-relative) and then
   re-checks the parsed origin as defense in depth.
3. **Client-address preservation (`upstreamHeaders`).** The BFF now relays
   `X-Forwarded-For` / `X-Real-IP` upstream, so the apiserver's per-IP login limiter
   (`internal/apiserver/authroutes.go` → `auth.ClientIP`) attributes attempts to the
   real caller instead of the shared BFF pod address — otherwise five failed logins
   would lock every user behind that pod. **Deployment requirement:** the apiserver's
   `trustedProxies` (config.go) MUST be set to the BFF pod/Service CIDR only, so a
   client-forged `X-Forwarded-For` cannot poison another user's bucket. This is an
   ops/Helm-values concern tracked as a follow-up, not a code change in this PR.

## Consequences

- ✅ `/login` is a real, shell-free route; the 404 no longer leaks the operator
  rail; `<ConsoleShell>` is defined once.
- ✅ No authorization semantics changed — pure routing + one cookie-relaying
  proxy. The apiserver remains the sole credential verifier and cookie issuer.
- ⚠️ Any *new* authenticated page must be created **under `console/app/(app)/`**
  to get the shell. A page added at the app root would render shell-free (that is
  now the pre-auth surface).
- 📌 Verified: `next build` (all authed URLs unchanged, `/login` static), console
  vitest suite + `test/session/login.test.ts` (Set-Cookie relay, 204 logout,
  login-CSRF rejection, XFF forwarding) and `test/session/next-redirect.test.ts`
  (open-redirect / backslash-bypass regression).

## Files

- `console/app/layout.tsx` (bare), `console/app/(app)/layout.tsx` (shell),
  `console/app/not-found.tsx`
- `console/app/login/page.tsx` (branded markup owned by ISI-3523)
- `console/app/api/session/route.ts` (POST/DELETE added), `console/lib/bff.ts`
  (`proxyAuth`, `crossSiteReject`, XFF forwarding)
- `console/test/session/login.test.ts`, `console/test/session/next-redirect.test.ts`
