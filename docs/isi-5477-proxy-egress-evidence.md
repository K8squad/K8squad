# ISI-5477 — shim HTTPS_PROXY/NO_PROXY injection: empirical evidence

Gap #3 of ISI-5473. The acceptance-critical unknown was whether opencode's
model `fetch` (bun-compiled `sst/opencode` v1.18.27, `@ai-sdk/openai-compatible`
→ Bun `fetch`) honors `HTTPS_PROXY`/`NO_PROXY` natively, or whether a custom
`undici`/`global-agent` `ProxyAgent` dispatcher would be required.

**Result: Bun's `fetch` honors `HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY` natively.
No in-process dispatcher is needed — env injection alone routes the model call.**

## Method

Tested against the **exact production binary**: `opencode-linux-x64` v1.18.27
(the glibc sibling of the `-musl` tarball `Dockerfile.shim:78-95` stages; both
are bun-compiled with the same Bun runtime, so `fetch` proxy behavior is
identical). A minimal logging CONNECT proxy recorded every tunnel it received
and forwarded to the real upstream, so the model request completed naturally.

opencode config mirrored the repo's BYO render
(`capability.RenderOpenCodeConfig`): `provider.ksquad-byo` with
`npm: @ai-sdk/openai-compatible`, `options.baseURL: https://api.openai.com/v1`,
one model `gpt-proxy-test`. A dummy (non-secret) `OPENCODE_API_KEY` was used;
OpenAI's genuine `401` (with Cloudflare `cf-ray` / `x-openai-proxy-wasm`
headers and a real JSON error body) proves the request reached the real
upstream through the proxy.

## Scenario A — `HTTPS_PROXY` set, `NO_PROXY` empty → model fetch egresses via proxy

Env: `HTTPS_PROXY=http://127.0.0.1:8899`, `HTTP_PROXY=http://127.0.0.1:8899`.

Proxy log:

```
[15:20:37.485] CONNECT registry.npmjs.org:443  <-- HTTPS egress via proxy
[15:20:37.571] CONNECT api.openai.com:443       <-- HTTPS egress via proxy
```

opencode received a real upstream response through the tunnel:

```
"statusCode":401 ... "metadata":{"url":"https://api.openai.com/v1/chat/completions"}
responseHeaders: cf-ray=a45cb98d7949e9ab-MRS, x-openai-proxy-wasm=v0.1, server=cloudflare
```

→ The `@ai-sdk/openai-compatible` model POST to `api.openai.com` **tunneled
through the proxy** (CONNECT logged) and reached the real OpenAI edge.

## Scenario B — `HTTPS_PROXY` set, `NO_PROXY=api.openai.com` → model fetch bypasses proxy

Env adds `NO_PROXY=api.openai.com` against the same live proxy (port 8920).

Proxy log:

```
[15:23:00.324] logproxy listening on 127.0.0.1:8920
[15:23:02.035] CONNECT registry.npmjs.org:443  <-- HTTPS egress via proxy
```

opencode still reached OpenAI (direct):

```
"statusCode":401
"url":"https://api.openai.com/v1/chat/completions"
```

→ With `api.openai.com` in `NO_PROXY`, the model fetch went **direct** (no
`api.openai.com` CONNECT in the proxy log), while traffic to a host NOT in
`NO_PROXY` (`registry.npmjs.org`) still routed through the proxy. Per-host
`NO_PROXY` bypass works.

## Conclusion / design

- Both acceptance halves proven: external model fetch egresses via the proxy;
  `NO_PROXY`-listed targets (in-cluster services, LAN BYO endpoints) go direct.
- Mechanism is **pure env injection**. Implemented at the universal choke point
  `pkg/shim/runner.go` (`agentProxyEnv`): the shim reads namespaced
  `KSQUAD_SANDBOX_{HTTPS,HTTP,NO}_PROXY` from its own pod env and stamps the
  conventional `HTTPS_PROXY`/`HTTP_PROXY`/`NO_PROXY` (both cases) onto the agent
  child only — covering both the operator-spawned `shim run` and the in-sandbox
  supervisor topologies, without proxying the operator/supervisor's own egress.
- Operator propagation: `cmd/operator/main.go` passes the namespaced vars to
  sandbox pods via `WithPodEnv`; Helm `operator.sandbox.proxy.*`
  (`deploy/helm/ksquad`) sets them on the operator pod and fails the render if
  `httpsProxy` is set without a `noProxy` (prevents swallowing in-cluster
  egress).

## Reproduce

Harness scripts are not committed (throwaway). To reproduce: download opencode
v1.18.27, run a logging CONNECT proxy, point an `@ai-sdk/openai-compatible` BYO
provider at an HTTPS endpoint, set `HTTPS_PROXY`, and observe the proxy log vs
`NO_PROXY`. Automated coverage lives in
`pkg/shim/runner_test.go` (`TestAgentProxyEnv`,
`TestOSRunnerInjectsProxyEnvIntoChild`, `TestOSRunnerNoProxyEnvWhenUnset`).
