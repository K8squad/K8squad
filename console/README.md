# ksquad-console

The KSquad **operator console** — Next.js (App Router) UI + **BFF** (SSE fan-out). This is the
Epic 8 shell: the route/build tooling every 8.x feature screen mounts into, the one authorization
choke point (arch §13 / ADR-013), the one SSE bus (story 8.2), and whole-shell theming (story 8.9).

> **Scope of this scaffold (ISI-2180).** The _shell_, not the 11 feature screens. It lands the
> load-bearing infrastructure the feature stories were blocked on: build tooling, the App Router,
> the BFF proxy, the shared EventSource client, and the dark/light theme. Feature screens (8.1
> squad overview, 8.3–8.11, 10.3 discussion room) build on top of it.

## What's here

```
console/
  app/
    layout.tsx                 shell (nav topbar + 8-Crest logo + theme toggle)
    page.tsx                   landing / squad-overview mount point (8.1)
    globals.css                token-driven styling (dark ⇄ light)
    runs/[runId]/page.tsx      Run detail — live SSE timeline (8.2)
    api/
      runs/[runId]/stream/route.ts              BFF SSE proxy — the ONE bus (8.2)
      runs/[runId]/build/[resource]/route.ts    BFF build-browser proxy, 404-verbatim (8.7d)
  components/
    ThemeProvider.tsx / ThemeToggle.tsx         whole-shell theming (8.9)
    RunStream.tsx                               live timeline, read-only (8.2)
    Logo.tsx                                     v2 8-Crest lockup (8.9)
  lib/
    bff.ts                     server-only apiserver proxy (SSE + JSON), identity forwarding
    useRunStream.ts            the ONE shared EventSource client (no polling)
    theme.ts                   DARK/LIGHT token maps (mirrors console_kit.py)
```

## Architectural invariants (do not regress)

- **One choke point (ADR-013).** The browser talks ONLY to this Next.js server. `lib/bff.ts` is
  `server-only` and holds the apiserver URL; it is never exposed to the client (no `NEXT_PUBLIC_*`).
- **One SSE bus, no polling (story 8.2).** Every live surface uses `lib/useRunStream` — one native
  `EventSource` against the BFF route. Do **not** add a second SSE client or a polling loop.
- **Unbuffered stream (8.2 AC3).** The SSE proxy streams the upstream body through incrementally
  and sets `X-Accel-Buffering: no`.
- **404, never 403 (8.7d / NFR-SEC5).** The build-browser proxy surfaces the apiserver's status
  verbatim; a deny is existence-hiding. The authoritative per-principal gate lives in the Go
  apiserver read-model, not the BFF.
- **Read-only stream (8.2 AC6).** No mutate/claim/kill affordance rides the feed. Kill Run (FR-F4)
  is a separate control-plane action (story 3.3 / 8.4).
- **Theme = token swap (8.9).** Light mode is the dark shell with token roles luminance-inverted;
  the accent and reserved status hues are theme-invariant.

## Develop

```bash
cd console
npm install
KSQUAD_APISERVER_URL=http://localhost:8080 npm run dev   # http://localhost:3000
npm run build        # production build → .next/standalone (Dockerfile.console runtime)
npm run typecheck    # tsc --noEmit
```

Config: copy `.env.example`. `KSQUAD_APISERVER_URL` (server-only) points the BFF at the Go
apiserver; `KSQUAD_SESSION_COOKIE` names the HttpOnly session cookie the BFF forwards upstream.

## Container

`Dockerfile.console` (repo root) builds this via `output: 'standalone'` onto distroless node24.
