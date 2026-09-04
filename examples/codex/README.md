# Codex example

A minimal, applyable example of a single agent running on the **Codex**
runtime — OpenAI's official Rust coding agent (epic ISI-3647, arch ISI-3646).
It mirrors the shape of [`../bmad-team`](../bmad-team) reduced to one runtime,
one credential, one role and one agent, so it reads as the smallest end-to-end
Codex spec set.

## What's here

| File | Kind | Purpose |
|------|------|---------|
| `00-namespace.yaml`   | Namespace     | `codex-squad` — everything lives here |
| `01-credentials.yaml` | Secret        | BYO OpenAI API key (`OPENAI_API_KEY`), service-account |
| `02-runtime.yaml`     | AgentRuntime  | `type: codex`, `cliVersion: rust-v0.152.0` (conformant, no experimental flag) |
| `04-prompt.yaml`      | ConfigMap     | behavior prompt referenced by the Role |
| `05-role.yaml`        | Role          | `codex-coder`, referencing the prompt |
| `06-agent.yaml`       | Agent         | `cody`, binding the model + BYO credential to the Role and Runtime |

## Key points

- **Conformant runtime.** `codex` is in the built-in runtime set, so the
  AgentRuntime admits **without** `spec.experimental=true`. Setting the
  experimental flag would wrongly mark the generated Agent Card as a vendor
  shim.
- **BYO credential, service-account class.** `spec.credentialClass:
  service-account` tells the credential-injection contract
  (`pkg/credinject`) to map the referenced Secret onto the runtime-native env
  var `OPENAI_API_KEY`. There is no human-seat path for codex in v1 — a
  human-seat class on a codex Agent fails closed (ISI-3647 S9).
- **Default model & endpoint.** `spec.model: gpt-5.4-codex` is the runtime's
  default model; no `modelEndpointRef` is set, so the Agent uses the default
  OpenAI endpoint.

## Apply

```sh
# 1. Put a real OpenAI API key in 01-credentials.yaml (replace REPLACE_ME).
# 2. Apply in order — the namespace first, then the rest.
kubectl apply -f examples/codex/
```

Once reconciled, a Codex `AgentRuntime` and `Agent` are created and admitted as
conformant (no experimental flag).
