# Runbook — Credential rotation & the zero-touch OAuth controller

**Scope:** Epic 7 credentials (stories 7.1–7.8). Covers the two credential
**classes** and the leader-elected credential controller (story 7.7, arch
§5.2 / §11.1, ADR-032 / ADR-041) that keeps human-seat OAuth tokens fresh with
zero operator touch.

Owner: platform on-call. Related: `pkg/controller/credential`,
`pkg/credinject`, `pkg/credential`, console screen 05 (story 8.6).

---

## 0. The two credential classes (ADR-041)

Every `Agent` references a **per-user Kubernetes Secret** (never a shared
master, ADR-010) and declares a **class** via `spec.credentialClass`:

| Class | Meaning | Rotation model | Controller involvement |
|-------|---------|----------------|------------------------|
| `human-seat` | Interactive Claude OAuth token bound to a person's subscription seat (story 7.2/7.7). | **Zero-touch** — the controller auto-refreshes the ~8h access token in place. Re-login only if the ~9-day refresh window lapses. | **Yes** — the credential controller owns refresh. |
| `service-account` | Long-lived API key / provider token, no interactive OAuth (story 7.3). Default when `credentialClass` is unset. | **Manual** — rotation = update the Secret's key. | **No** — the controller never touches it. |

The controller **only** watches Secrets labelled
`ksquad.io/credential-class: human-seat`. A service-account credential is
structurally excluded from the OAuth lifecycle — this is the ADR-041
enforcement, enforced by the Watch predicate, not by a runtime check.

---

## 1. Human-seat OAuth Secret shape

A human-seat OAuth Secret carries (see `pkg/controller/credential/secret.go`):

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: claude-oauth-<user>
  namespace: <squad-namespace>
  labels:
    ksquad.io/credential-class: human-seat   # selects the controller
data:
  token:         <access token>   # the ~8h OAuth access token — agent pods mount this
  refreshToken:  <refresh token>  # controller-only; never mounted by pods
  expiresAt:     <RFC3339>         # access-token expiry the controller refreshes against
  connectedAt:   <RFC3339>         # one-time-login timestamp (best-effort)
```

- Agent pods mount **only** `token` (as `CLAUDE_CODE_OAUTH_TOKEN`, via
  `pkg/credinject`). Many pods sharing one Secret run concurrently.
- The control plane **never reads** `token`/`refreshToken` for injection —
  injection is by `SecretKeySelector` reference (NFR-SEC3). The **only** reader
  of `refreshToken` is the credential controller, on the elected leader.

### Health surface (console screen 05 / story 8.6)

The controller stamps annotations — the operator sees status, never token bytes:

| Annotation | Values |
|------------|--------|
| `ksquad.io/credential-state` | `connected` · `refreshing` · `expired` · `error` |
| `ksquad.io/credential-expires-at` | RFC3339 access-token expiry |
| `ksquad.io/credential-last-refresh` | RFC3339 last successful refresh (canary heartbeat) |
| `ksquad.io/credential-message` | short human-readable reason |

---

## 2. Zero-touch refresh — how it works (no action needed)

1. User connects once: console **"Connect Claude"** browser OAuth **or** CLI
   `ksquad auth login`. The flow writes the Secret above.
2. The **leader-elected** credential controller (one refresher across all
   operator replicas — leader election guarantees no thundering-refresh race)
   watches expiry and, ~30 min before the access token expires
   (`DefaultRefreshLead`, tunable), exchanges the refresh token for a new
   access token and **writes it back to the SAME Secret** in place.
3. Concurrent agent pods pick up the new `token` on the next kubelet Secret
   projection — no pod restart, no manual re-token.
4. State returns to `connected`; `last-refresh` advances.

**You do nothing.** This section exists so you recognise healthy behaviour.

---

## 3. Alert: credential `expired` (human-seat)

**Symptom:** `ksquad.io/credential-state: expired`; affected Runs pause
`Paused(cred_expired)` (story 7.4); console offers **one-click re-login**.

**Cause:** the refresh token itself was rejected (OAuth `invalid_grant`) — the
~9-day refresh window lapsed (e.g. a long-idle seat). This is the **only**
terminal case; transient network/5xx failures do **not** expire a credential.

**Resolution (the user, or on their behalf):**
1. Console screen 05 → the expired agent → **Re-login** (one-click browser
   OAuth), **or** run `ksquad auth login` for that seat.
2. The login rewrites `token` / `refreshToken` / `expiresAt` and clears the
   expired state.
3. Paused Runs resume automatically on the refreshed Secret (story 7.4). No pod
   restart required.

Do **not** hand-edit the Secret to fake a token — the refresh token is only
mintable through the OAuth flow.

---

## 4. Alert: credential `error` (human-seat)

**Symptom:** `ksquad.io/credential-state: error`, message names a missing/bad
field.

**Cause:** the Secret is labelled `human-seat` but is missing `refreshToken` or
`expiresAt`, or `expiresAt` is unparseable — a provisioning bug, not a lapsed
window.

**Resolution:** re-run the one-time login (§3.1) to rewrite a well-formed
Secret. Correcting the Secret triggers an immediate reconcile.

---

## 5. Rotating a service-account credential (manual)

Service-account keys have **no** auto-refresh — rotation is a Secret update:

```bash
kubectl -n <squad-namespace> patch secret <name> \
  --type merge -p '{"stringData":{"apiKey":"<new key>"}}'
```

Agent pods pick up the new key on the next Secret projection. If a Run was mid-
flight on the old key it pauses/resumes per story 7.4. **Never** add the
`human-seat` label to a service-account Secret — it has no refresh token and
would land in `error`.

### 5.1 Codex (OpenAI) — service-account only in v1

Codex (`AgentRuntime{type: codex}`, the official Rust `codex` CLI) authenticates
with a **BYO OpenAI API key** and is therefore a plain **`service-account`**
credential — rotate it exactly as §5 above. Codex-specific details
(`pkg/credinject`, `pkg/shim/runtimes/codex.go`):

- The per-user Secret's `apiKey` value is injected by reference as
  **`OPENAI_API_KEY`** (the OpenAI-standard env, ADR-026 OpenAI wire). Rotation
  is the same `kubectl patch secret … stringData.apiKey` as §5.
- **Human-seat (ChatGPT-subscription) auth is not available in v1.** It is a
  ToS-gated fast-follow (ISI-3661). A `credentialClass: human-seat` on a `codex`
  Agent **fails closed** — the pair is deliberately unmapped, so no Run
  authenticates under an env the codex CLI ignores. Use `service-account`.
- **Egress:** codex Run pods need network egress to **`api.openai.com`** (or, for
  a BYO OpenAI-compatible endpoint, that host) — allow it in the squad
  NetworkPolicy. A BYO endpoint carries its own token + base URL via the model
  route (story 5.7); the per-user `OPENAI_API_KEY` is emitted only when no BYO
  route is set.

---

## 6. Controller tuning (arch §21 — not a v1 gate)

- **Refresh lead** (`Reconciler.RefreshLead`, default 30 min) — how early the
  token is refreshed before expiry. Raise it if the provider endpoint is flaky.
- **OAuth endpoint / client ID** (`AnthropicRefresher.TokenURL` / `.ClientID`)
  — operator-overridable; defaults to the known Claude Code OAuth values.
- **Transient retry backoff** (`DefaultErrorRequeue`, 1 min) — retry cadence
  after a network/5xx refresh blip.

## 7. Verifying the controller is healthy

```bash
# Leader (the single active refresher):
kubectl -n ksquad-system get lease | grep ksquad
# Per-credential health at a glance:
kubectl get secret -A -l ksquad.io/credential-class=human-seat \
  -o custom-columns=NS:.metadata.namespace,NAME:.metadata.name,\
STATE:.metadata.annotations.ksquad\\.io/credential-state,\
EXPIRES:.metadata.annotations.ksquad\\.io/credential-expires-at,\
LASTREFRESH:.metadata.annotations.ksquad\\.io/credential-last-refresh
```

A `connected` state with a `last-refresh` that keeps advancing ahead of
`expires-at` is the healthy steady state.
