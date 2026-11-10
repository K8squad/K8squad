# Supported Agent Runtimes

KSquad supports multiple conformant agent runtimes, allowing you to mix different AI models and tools within the same squad. Each runtime has specific capabilities, credential requirements, and usage patterns.

## Supported Runtimes

The CRD admits exactly these conformant runtime types (see the `spec.type` validation in `api/v1alpha1/agentruntime_types.go`); anything else requires `spec.experimental=true` and is treated as a vendor shim:

| Runtime | Type | Status | Credential Class | Description |
|---------|------|--------|------------------|-------------|
| **claude-code** | Conformant | ✅ Stable | `human-seat` ⚡, `service-account` | Anthropic's Claude Code CLI for coding assistance |
| **codex** | Conformant | ✅ Stable | `service-account` only 🔄 | OpenAI's official Rust coding agent (GPT models) |
| **opencode** | Conformant | ✅ Stable | `service-account` | Open-source terminal coding agent (opencode) |
| **openclaw** | Conformant | ✅ Stable | `service-account` | Anthropic's Claude CLI (open-source variant) |
| **hermes** | Conformant | ✅ Stable | `service-account` | Custom runtime implementation |
| **vibe** | Conformant | ✅ Stable | `service-account` (Mistral API key) | Mistral's `vibe` coding agent |
| **goose** | Conformant | 🧪 Preview | `service-account` (provider API key) | Block's open-source `goose` coding agent, one-shot stream-json engine |
| **crewai** | Conformant | 🧪 Alpha | `service-account` (`AUTH_TOKEN`) | CrewAI multi-agent framework runtime, always-on mesh pool |

All eight types are conformant: none of them needs `spec.experimental=true`. The status column tracks
engine and console maturity, not CRD admission — anything other than ✅ Stable means expect rougher
edges and possible behavior changes between releases.

> **Local models (Ollama and friends):** there is no `ollama` runtime *type*. Local or OpenAI-compatible endpoints are routed per-runtime via `spec.modelEndpointRef`, which projects `OPENAI_BASE_URL` (and a placeholder token when none is needed) into the run. See [BYO credentials](./getting-started/getting-started-byo-cred.md#alternative-byo-endpoints).

## Runtime Details

### Claude Code (`claude-code`)

**Status**: Stable, conformant runtime  
**Credential Classes**: 
- `human-seat` ⚡ (recommended, auto-refreshed)
- `service-account` (manual rotation)

**Features**:
- Anthropic's official CLI for Claude models
- Zero-touch OAuth refresh (human-seat)
- Best-in-class coding assistance
- Human-seat auth supports an interactive, subscription-backed experience

**Usage**:
```yaml
apiVersion: ksquad.io/v1alpha1
kind: AgentRuntime
metadata:
  name: claude-code-runtime
spec:
  type: claude-code
  # cliVersion unset: the shim resolves the default channel (ADR-017).
  # Pin an immutable tag/SHA for reproducibility when you need it.
```

### Codex (`codex`)

**Status**: Stable, conformant runtime  
**Credential Classes**: 
- `service-account` only 🔄 (no human-seat in v1)

**Features**:
- OpenAI's official Rust coding agent
- Supports GPT-4, GPT-4o, GPT-5 models
- Native OpenAI wire protocol integration
- BYO OpenAI API key required

**Important Notes**:
- Human-seat (ChatGPT subscription) auth is not available in v1
- Uses `OPENAI_API_KEY` environment variable
- Requires network egress to `api.openai.com` (or custom endpoint)

**Usage**:
```yaml
apiVersion: ksquad.io/v1alpha1
kind: AgentRuntime
metadata:
  name: codex-runtime
spec:
  type: codex
  cliVersion: rust-v0.152.0  # pins the official Rust codex revision
```

### OpenCode (`opencode`)

**Status**: Stable, conformant runtime  
**Credential Classes**: 
- `service-account` (BYO provider API key)

**Features**:
- Open-source terminal-based coding agent
- Provider-agnostic: works with Anthropic, OpenAI, and other backends
- Same envelope contract (`KSQUAD_SYSTEM_CONTEXT` / `KSQUAD_INPUT`) as the other v1 runtimes

**Usage**:
```yaml
apiVersion: ksquad.io/v1alpha1
kind: AgentRuntime
metadata:
  name: opencode-runtime
spec:
  type: opencode
  # cliVersion unset: the shim resolves the default channel.
```

### OpenClaw (`openclaw`)

**Status**: Stable, conformant runtime  
**Credential Classes**: 
- `service-account` (Anthropic API key required)

**Features**:
- Anthropic's open-source CLI variant
- Compatible with Anthropic API models
- Local execution with remote API calls

**Usage**:
```yaml
apiVersion: ksquad.io/v1alpha1
kind: AgentRuntime
metadata:
  name: openclaw-runtime
spec:
  type: openclaw
```

### Hermes (`hermes`)

**Status**: Stable, conformant runtime  
**Credential Classes**: 
- `service-account` (custom authentication)

**Features**:
- Custom runtime implementation
- Extensible architecture
- Plugin support for various models

**Usage**:
```yaml
apiVersion: ksquad.io/v1alpha1
kind: AgentRuntime
metadata:
  name: hermes-runtime
spec:
  type: hermes
```

### Vibe (`vibe`)

**Status**: Stable, conformant runtime (dedicated engine; full console LLM-settings tab)  
**Credential Classes**: 
- `service-account` only — the Mistral API key rides `MISTRAL_API_KEY` (secret key `apiKey` by default)

**Features**:
- Mistral's `vibe` coding agent, served by a dedicated shim engine
- Mistral provider row in the console LLM settings (API-key mode, OpenAI-compatible wire)
- Optional base-URL override for BYO / self-hosted Mistral-compatible endpoints (default `https://api.mistral.ai/v1`)

**Important Notes**:
- Service-account only in v1: a human-seat credential class fails closed at injection time
- The base-URL override is a model-endpoint concern, not part of the credential class

**Usage**:
```yaml
apiVersion: ksquad.io/v1alpha1
kind: AgentRuntime
metadata:
  name: vibe-runtime
spec:
  type: vibe
```

### Goose (`goose`)

**Status**: Preview, conformant runtime (engine + mesh adapter shipped; console LLM-settings tab is preview)  
**Credential Classes**: 
- `service-account` (provider API key; the openai provider lane consumes `OPENAI_API_KEY`)

**Features**:
- Block's open-source `goose` coding agent as a one-shot `goose run` stream-json engine in the shim
- Native mesh-side adapter — goose participates in the AgentMesh like any other runtime
- Headless by construction: keyring access is disabled (`GOOSE_DISABLE_KEYRING=1`), credentials arrive via the standard injection envelope

**Important Notes**:
- Provider selection is derived from the model route; the injected credential is projected onto the matching provider key
- Preview status: prefer it for non-critical squads until it stabilizes

**Usage**:
```yaml
apiVersion: ksquad.io/v1alpha1
kind: AgentRuntime
metadata:
  name: goose-runtime
spec:
  type: goose
```

### CrewAI (`crewai`)

**Status**: Alpha, conformant runtime (adapter #5 wiring; zero control-plane change)  
**Credential Classes**: 
- `service-account` (`AUTH_TOKEN`, shared by the crew's A2A router lane and the standing child)

**Features**:
- The CrewAI multi-agent framework as an **always-on pool** runtime: instead of one-shot dispatch, a standing sandbox hosts the crew and the mesh routes work to it
- Dedicated `python + uv + crewai[a2a]` sandbox image
- Wired through the gateway's `crewaiRouter` lane into the AgentMesh

**Important Notes**:
- Alpha: the wiring is complete but the surface is young; expect iteration
- Unlike the one-shot coding runtimes, a crewai runtime holds long-lived state in its sandbox

**Usage**:
```yaml
apiVersion: ksquad.io/v1alpha1
kind: AgentRuntime
metadata:
  name: crewai-runtime
spec:
  type: crewai
```

## Mixed Runtime Squads

You can mix different runtimes within the same squad, allowing for specialized roles:

```yaml
kind: Team
metadata:
  name: mixed-runtime-squad
spec:
  namespace: mixed-squad
  # ... other team config
---
kind: Agent
metadata:
  name: claude-architect
spec:
  runtimeRef: claude-code-runtime
  roleRef: architect-role
  credentialSecretRef:
    name: claude-credentials
    key: token
---
kind: Agent
metadata:
  name: codex-developer
spec:
  runtimeRef: codex-runtime
  roleRef: developer-role
  credentialSecretRef:
    name: openai-credentials
    key: token
```

## Runtime Selection Guide

### Choose Claude Code if:
- You need the most advanced coding assistance
- You have an Anthropic plan and want zero-touch credential management (human-seat)
- You're building complex software systems

### Choose Codex if:
- You're already invested in the OpenAI ecosystem
- You need GPT-4/5 model access
- You prefer BYO API key management
- You're working with OpenAI-compatible endpoints

### Choose OpenCode if:
- You want an open-source, provider-agnostic coding agent
- You need to swap model providers without changing runtime types
- You're aligning with the same tooling your local teams already use

### Choose OpenClaw if:
- You need Anthropic model access but prefer CLI tooling
- You want a lighter CLI implementation
- You're working in environments with Anthropic API access

### Choose Hermes if:
- You need custom runtime features
- You want maximum flexibility
- You're building custom integrations
- You need plugin support

### Choose Vibe if:
- You're standardized on Mistral models
- You want a conformant runtime with a first-class console onboarding tab
- BYO Mistral-compatible endpoints matter to you

### Choose Goose if:
- You want Block's open-source agent with minimal lock-in
- You like one-shot stream-json semantics with full mesh participation
- You're comfortable on a preview surface

### Choose CrewAI if:
- You're orchestrating *multi-agent crews* rather than a single coding agent
- You want long-lived, stateful crews held in a warm sandbox
- You're prepared for alpha-level iteration

## Configuration Notes

- All conformant runtimes work without the `experimental=true` flag
- Experimental runtimes require explicit `spec.experimental=true` and may be marked as vendor shims
- Each runtime has specific toolchain and dependency requirements
- Network policies may need adjustment depending on the runtime's external dependencies

## Runtime Updates

- CLI versions are tracked in the respective runtime manifests
- Monitor the main KSquad repo for runtime updates and deprecation notices
- Check the [examples directory](../examples) for the latest runtime configurations

For detailed setup instructions for each runtime, see the specific getting started guides in the [getting-started directory](./getting-started/).
