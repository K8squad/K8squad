# BYO model endpoints (LLM Settings)

K8squad lets a cluster admin wire a Bring-Your-Own LLM provider (a LAN Ollama box,
Z.ai, DeepSeek, Kimi/Moonshot, or any OpenAI-compatible gateway) without
hand-crafting a Kubernetes Secret. The flow is: **pick a provider → list its live
models → pick one → save**. Saving upserts a single labelled endpoint Secret that
the model resolver ([model-per-role](model-per-role.md)) reads.

> **Status (ISI-4989 rework).** The backend model-endpoint surface (ISI-5005) and
> the console provider picker (ISI-5006) have landed. The picker renders today
> inside the Settings → Configuration screen (via the Model Priority section) and
> the "compose an agent" runtime-adapter step. A **dedicated `/settings/llm` nav
> entry and route (ISI-5004)** is tracked separately — see the follow-up defect
> filed under the QA gate ISI-5007 — so this page documents the contract, not the
> final navigation surface.

## Provider registry

The apiserver owns a data-only provider registry. Each provider declares how it is
addressed (`mode`) and which model-listing dialect it speaks (`wire`):

| Provider ID         | Label                        | Mode     | Wire   | Base URL / notes                              |
|---------------------|------------------------------|----------|--------|-----------------------------------------------|
| `ollama`            | Ollama (local)               | `url`    | ollama | Caller supplies the LAN URL; no key           |
| `zai`               | Z.ai                         | `apiKey` | openai | `https://api.z.ai/api/paas/v4`                |
| `deepseek`          | DeepSeek                     | `apiKey` | openai | `https://api.deepseek.com`                    |
| `kimi-moonshot`     | Kimi (Moonshot)              | `apiKey` | openai | `https://api.moonshot.cn/v1`                  |
| `openai-compatible` | OpenAI-compatible (custom)   | `url`    | openai | Caller supplies URL; may also carry a key     |
| `anthropic`         | Anthropic (Claude)           | curated  | —      | Rendered from a curated list; never dialed    |
| `openai`            | OpenAI (Codex)               | curated  | —      | Rendered from a curated list; never dialed    |

- **`mode: url`** — the caller supplies the base URL (LAN Ollama, generic
  OpenAI-compatible gateway). Private/LAN ranges are allowed **by design**.
- **`mode: apiKey`** — the provider has a fixed hosted base URL; the caller
  supplies only the API key.
- **`wire: ollama`** lists models with `GET {base}/api/tags`; **`wire: openai`**
  uses `GET {base}/models`.
- **Curated-only** providers (Claude, Codex) are rendered by the console from a
  curated list. This surface never dials or stores a Secret for them — their
  credentials ride the existing setup-token / OAuth flow.

## API surface

All three routes are **session-gated and admin-tier**. An authenticated non-admin
gets `403`; an unauthenticated caller gets `401`.

| Verb + path                              | Purpose                                                        |
|------------------------------------------|---------------------------------------------------------------|
| `GET  /api/modelendpoints/providers`     | The provider registry (IDs, labels, modes) for the picker.    |
| `POST /api/modelendpoints/list-models`   | `{provider, url?, apiKey?}` → live model list from the provider. |
| `POST /api/modelendpoints`               | `{name?, provider, url?, apiKey?, model?}` → upsert endpoint Secret. |
| `GET  /api/modelendpoints`               | List configured endpoints (metadata only — **never** the key). |

**Security invariants (enforced by handler tests in
`internal/apiserver/modelendpoints_test.go`):**

- The API key is **never echoed** — not in a `list-models` response, not on
  `create`, not on `list`.
- Every verb is admin-tier (403 for authenticated non-admins, 401 unauthenticated).
- A LAN / loopback URL is allowed by design (BYO Ollama).
- The listing HTTP client refuses redirects and is byte- and time-bounded.

## Endpoint Secret contract

A save lands **one** Kubernetes Secret in the operator namespace
(`k8squad-system` by default), keyed to the story 7.5 BYO-endpoint shape:

| Secret data key | Required | Meaning                                             |
|-----------------|----------|-----------------------------------------------------|
| `endpointURL`   | yes      | The provider base URL (parseable http(s) with host). |
| `apiToken`      | no       | The API key. Omitted for a keyless LAN Ollama endpoint. |

Read-side aliases `url` / `token` are also accepted by the resolver. The Secret
carries these labels so the resolver and tooling can find it:

| Label                                | Value       |
|--------------------------------------|-------------|
| `ksquad.io/model-endpoint`           | `true`      |
| `ksquad.io/model-endpoint-provider`  | `<provider>`|

The write target namespace and the resolver's read target namespace are the same
value, so they can never drift (`NewModelEndpointService` defaults an empty
namespace to the resolver's `DefaultSystemNamespace`).

## RBAC lockstep

The apiserver's Secret Get/Create/Update/List permissions for this surface are
pinned against the control-plane RBAC in `config/helm/rbac_lockstep_test.go`
(`config/helm/templates/control-plane/rbac.yaml`), so a new route verb and its
Helm grant move together.

## Round-trip

1. Pick a provider (e.g. `ollama`) and enter the URL (e.g. `http://10.0.0.5:11434`).
2. `list-models` dials the provider and returns its live model names.
3. Pick a model and save → one labelled endpoint Secret is upserted.
4. The [model resolver](model-per-role.md) reads that Secret + the `default`
   ModelConfig singleton to resolve the effective `(primary, fallback)` model,
   failing closed (`ErrNoModel`) when no tier yields a model.

Claude and Codex are unaffected by this surface: they remain curated-only and
continue to use their existing credential flow.
