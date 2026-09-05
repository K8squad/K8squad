# Supported Agent Runtimes

KSquad supports multiple conformant agent runtimes, allowing you to mix different AI models and tools within the same squad. Each runtime has specific capabilities, credential requirements, and usage patterns.

## Supported Runtimes

| Runtime | Type | Status | Credential Class | Description |
|---------|------|--------|------------------|-------------|
| **claude-code** | Conformant | ✅ Stable | `human-seat` ⚡, `service-account` | Anthropic's Claude Code CLI for coding assistance |
| **codex** | Conformant | ✅ Stable | `service-account` only 🔄 | OpenAI's official Rust coding agent (GPT models) |
| **ollama** | Conformant | ✅ Stable | `service-account` | Local open-source model runner |
| **openclaw** | Conformant | ✅ Stable | `service-account` | Anthropic's Claude CLI (open-source variant) |
| **hermes** | Conformant | ✅ Stable | `service-account` | Custom runtime implementation |

## Runtime Details

### Claude Code (`claude-code`)

**Status**: Stable, conformant runtime  
**Credential Classes**: 
- `human-seat` ⚡ (recommended, auto-refreshed)
- `service-account` (manual rotation)

**Features**:
- Anthropic's official CLI for Claude 3.5 Sonnet
- Zero-touch OAuth refresh (human-seat)
- Best-in-class coding assistance
- Human-seat auth supports interactive ChatGPT-style experience

**Usage**:
```yaml
apiVersion: ksquad.io/v1alpha1
kind: AgentRuntime
metadata:
  name: claude-code-runtime
spec:
  type: claude-code
  cliVersion: rust-v0.152.0
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
  cliVersion: rust-v0.152.0
```

### Ollama (`ollama`)

**Status**: Stable, conformant runtime  
**Credential Classes**: 
- `service-account` (local model access)

**Features**:
- Runs open-source models locally
- Supports Llama, Mistral, Mixtral, and others
- No external API dependencies
- Local model deployment required

**Usage**:
```yaml
apiVersion: ksquad.io/v1alpha1
kind: AgentRuntime
metadata:
  name: ollama-runtime
spec:
  type: ollama
  cliVersion: rust-v0.152.0
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
  cliVersion: rust-v0.152.0
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
  cliVersion: rust-v0.152.0
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
- You have access to Anthropic's ChatGPT-style interface
- You prefer zero-touch credential management
- You're building complex software systems

### Choose Codex if:
- You're already invested in the OpenAI ecosystem
- You need GPT-4/5 model access
- You prefer BYO API key management
- You're working with OpenAI-compatible endpoints

### Choose Ollama if:
- You need offline/local execution
- You prefer open-source models
- You have privacy requirements
- You want to avoid external API dependencies

### Choose OpenClaw if:
- You need Anthropic model access but prefer CLI tooling
- You want a lighter CLI implementation
- You're working in environments with Anthropic API access

### Choose Hermes if:
- You need custom runtime features
- You want maximum flexibility
- You're building custom integrations
- You need plugin support

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
