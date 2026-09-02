# ADR-0006 — Login rate-limiter shared-IP DoS: credential-first gating + honest client IP

- **Status:** Accepted — *implementation delegated*
- **Date:** 2026-09-03
- **Author:** Winston (System Architect)
- **Issue:** ISI-3630 (surfaced by ISI-3629; earlier symptom ISI-3538)
- **Coordinates with:** ADR-0003 (console auth/login route + BFF), ADR-001 (no durable store)
- **Scope:** `pkg/auth` limiter ordering (Go), `console/lib/bff.ts` header forwarding (TS),
  `config/helm` + `deploy/helm/ksquad` `auth.trustedProxies` value, kgateway Envoy XFF
  config. NO new authorization surface — the apiserver stays the ONE authz choke point (§13).

## Context

The console login rate-limiter (`pkg/auth/ratelimit.go`, `pkg/auth/service.go`) is an
in-memory sliding window — default **5 FAILED attempts / 900s**, keyed by the caller's
client IP (`auth.ClientIP`). On k8squad-test the `ksquad-apiserver` Deployment ships with
`KSQUAD_TRUSTED_PROXIES` **unset**.

Two independent facts turn this brake into a **denial-of-service on all console logins**:

### Fact 1 — the limiter gates BEFORE credential verification

`Service.Login` (pkg/auth/service.go) runs `s.Limiter.Allow(ip)` as its **first** action and
returns `ErrRateLimited` → HTTP 429 before any password is checked. So once a bucket is full,
a request carrying the **correct admin password is rejected too**. `Success(ip)` clears the
window, but you can never reach it — `Allow` blocks first. Brute-force protection and
legitimate-login availability are coupled on the same gate.

### Fact 2 — every caller collapses into one shared bucket

`auth.ClientIP(trusted, xff, remoteAddr)` honors `X-Forwarded-For` **only** when the socket
peer is inside the trusted-proxy set; with an empty set it trusts no one and falls back to the
**socket peer** address. In this system the socket peer is never the browser:

```
browser (real client IP C)
   │  POST /login form
   ▼
kgateway / Envoy  ── proxies ──▶  console pod (Next.js BFF)  ── proxies ──▶  apiserver
   (Gateway 10.0.0.219)              /api/session → /auth/login              /auth/login
```

The login POST traverses the **console BFF** (ADR-0003: `/api/session` → apiserver
`/auth/login`). Therefore the apiserver's socket peer is the **console pod IP**, shared by
every browser. With `TrustedProxies` empty and no XFF, `ClientIP` returns that one pod IP for
everyone → **one shared bucket**. Any 5 failed logins from anyone (a bot scanning the gateway,
an earlier brute-force) lock out **all** users including admin, and the window re-trips easily
— which is why it never drained over 24h (observed twice: ISI-3538, ISI-3629).

### Why the ticket's first recommendation is insufficient

"Set `TrustedProxies` to the ingress CIDR" does **not** fix this topology on its own:

- The apiserver's peer is the **console pod**, not the gateway — trusting the gateway CIDR
  leaves `peer ∈ trusted` false, so XFF is never honored.
- The BFF does not forward XFF at all. `console/lib/bff.ts upstreamHeaders()` forwards only
  `cookie`, `last-event-id`, `accept`, `content-type` — **never `x-forwarded-for`**. So no
  per-client IP reaches the apiserver regardless of `TrustedProxies`.

## Decision

Three changes, ranked. **D1 alone removes the DoS**; D2/D3 restore *real* per-IP limiting.

### D1 (primary, topology-independent) — gate only the failure path

Reorder `Service.Login` so credentials are verified **first** and the limiter's `Allow` check
gates **only** the failure/consume path. A request with a **correct** password always
succeeds and calls `Success(ip)`; it is never rejected by a full bucket. Wrong passwords still
consume budget and are blocked after the limit — brute-force protection is unchanged.

Concretely (semantics, not final code):

- Verify username+password. On **success**: `Limiter.Success(ip)`, mint session, return — no
  `Allow` gate on the success path.
- On **failure**: if `!Allow(ip)` return `ErrRateLimited`; else `Failure(ip)` and return
  `ErrInvalidCredentials`.

This preserves the constant-time / opaque-error and dummy-hash timing-equalization behavior
already in `Login` (do the credential comparison unconditionally before branching). It keeps
ADR-001's in-memory store — no new dependency. Net effect: a shared-IP burst can **never**
lock out a user who knows their password, so the DoS is closed even if D2/D3 are imperfect.

**Tradeoff (must be weighed by Developer + Code Reviewer).** The current pre-hash `Allow`
gate has a *secondary* effect: it shields the expensive argon2id `VerifyPassword` from a
single-IP flood. Verifying credentials first removes that per-IP hash shield — a wrong-password
flood from one IP would run argon2id every attempt before being told 429/401. This is
**acceptable** because the real hash-cost shield is `MaxHashConcurrency` (the argon2id
semaphore, default 2), not the per-IP limiter — a per-IP limiter never shielded a multi-IP or
XFF-spoofed flood anyway. Confirm `MaxHashConcurrency` bounds concurrent derivations before
merging D1; if that semaphore is absent or unbounded, keep a cheap pre-hash gate for
*unknown users* (the `ByUsername` miss path, which already skips the real hash) and apply the
credential-first rule only to the known-user branch.

### D2 (secondary) — forward an honest client IP end-to-end

So the per-IP limiter actually keys per client (needed to brake a real distributed brute-force
against one shared BFF egress):

- **D2a — BFF forwards XFF.** `console/lib/bff.ts upstreamHeaders()` forwards the inbound
  `x-forwarded-for` (the chain Envoy stamped) to the apiserver. This is identity-adjacent
  metadata, not a BFF-asserted principal, so it does not violate the "forward identity only"
  rule — the apiserver still resolves authz from the session cookie alone; XFF feeds *only*
  the limiter key.
- **D2b — trust the real peer.** `auth.trustedProxies` = the **console pod (BFF) CIDR** (the
  apiserver's actual socket peer), set via `KSQUAD_TRUSTED_PROXIES`. The chart already wires
  this env from `.Values...auth.trustedProxies` (today `""`); the fix is the value.

### D3 (anti-spoof precondition) — gateway must stamp a trustworthy XFF

`ClientIP` walks the XFF **right→left** past trusted hops and returns the first untrusted
entry, so a client-spoofed `X-Forwarded-For: 9.9.9.9` is defeated **iff** Envoy appends the
true downstream address as the rightmost entry (`use_remote_address: true` /
`xff_num_trusted_hops`). ProxOps must confirm kgateway runs this before D2b is trusted;
otherwise XFF is attacker-controlled and D2b would let an attacker mint unlimited fresh
buckets — a limiter *bypass*, the opposite failure. Until D3 is confirmed, keep the
trust set empty and rely on D1.

### Operational immediate unblock (already used, reversible)

`kubectl -n k8squad-system rollout restart deploy/ksquad-apiserver` flushes the in-memory
counter (ADR-001). Login returns 200 immediately after. This is a mitigation, not a fix.

## Consequences

- **Availability:** correct-credential logins are never rate-limited (D1). The admin can
  always get in, ending the recurring lockout.
- **Security:** brute-force protection is preserved — wrong passwords still consume budget and
  are blocked. D3 is a hard precondition for D2b; shipping D2b without D3 is a regression.
- **Blast radius of D1:** small and local to `Service.Login`; covered by a table test asserting
  (a) correct password succeeds while the bucket is full, (b) wrong passwords still lock out
  after N, (c) `Success` still resets the window.
- **Config env note:** a shared-admin test env may also bump `loginRateLimit` / window, but D1
  makes that cosmetic rather than load-bearing.

## Delegation

| Change | Owner | Gate |
|--------|-------|------|
| D1 limiter reorder + test (`pkg/auth/service.go`) | Developer | none — ship first |
| D2a BFF XFF forwarding (`console/lib/bff.ts`) | Developer | none |
| D2b `auth.trustedProxies` = console-pod CIDR (values + live env) | ProxOps | after D3 confirmed |
| D3 verify kgateway Envoy `use_remote_address` | ProxOps | investigation |

Evidence + receipts: `/mnt/nas/project/k8squad-e2e-preserved/isi3629/RECEIPTS.md`.
