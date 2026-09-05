# Codex Agent Documentation

A comprehensive example of setting up KSquad with the **Codex** runtime — OpenAI's official Rust coding agent (epic ISI-3647, arch ISI-3646). This example demonstrates the complete setup from namespace creation to agent execution, with detailed explanations of Codex-specific considerations.

## What's Covered

This document provides:
- Complete setup walkthrough for Codex runtime
- Detailed credential management for BYO OpenAI keys
- Network configuration requirements
- Runtime-specific configuration options
- Security considerations
- Troubleshooting guide
- Advanced usage patterns

## Quick Start

```bash
# 1. Install KSquad operator
helm repo add ksquad https://charts.k8squad.io
helm install ksquad ksquad/k8squad --namespace k8squad-system --create-namespace

# 2. Create namespace and credentials
kubectl apply -f examples/codex/00-namespace.yaml
# Edit 01-credentials.yaml to add your OpenAI API key

# 3. Apply the rest of the configuration
kubectl apply -f examples/codex/

# 4. Verify deployment
kubectl -n codex-squad get team,agents,roles,runtimes

# 5. Open console and create runs
kubectl port-forward -n k8squad-system svc/ksquad-console 8080:80
```

## Complete Reference

| File | Kind | Purpose | Key Details |
|------|------|---------|-------------|
| `00-namespace.yaml` | Namespace | `codex-squad` | Isolation boundary for the squad |
| `01-credentials.yaml` | Secret | BYO OpenAI API key | Service-account class, `OPENAI_API_KEY` injection |
| `02-runtime.yaml` | AgentRuntime | Codex runtime configuration | Type: `codex`, CLI version: `rust-v0.152.0` |
| `04-prompt.yaml` | ConfigMap | Behavior prompt | Customizable agent behavior |
| `05-role.yaml` | Role | `codex-coder` definition | Skills, responsibilities, reporting structure |
| `06-agent.yaml` | Agent | Agent composition | Binds runtime + role + credentials |

## Detailed Configuration

### 1. Namespace and Credentials

```yaml
# 00-namespace.yaml
apiVersion: v1
kind: Namespace
metadata:
  name: codex-squad
```

```yaml
# 01-credentials.yaml
apiVersion: v1
kind: Secret
metadata:
  name: model-credentials
  namespace: codex-squad
  # Note: NO credential-class label for service-account
stringData:
  token: "sk-your-openai-api-key"  # This becomes OPENAI_API_KEY
type: Opaque
```

**Important Notes**:
- **Credential Class**: Codex uses `service-account` (no `human-seat` support in v1)
- **Environment Variable**: The `token` key is injected as `OPENAI_API_KEY`
- **Secret Key**: The key name (`token` in this case) must match what the Agent references
- **Security**: Never commit real API keys to version control

### 2. Runtime Configuration

```yaml
# 02-runtime.yaml
apiVersion: ksquad.io/v1alpha1
kind: AgentRuntime
metadata:
  name: codex-runtime
  namespace: codex-squad
spec:
  type: codex
  cliVersion: rust-v0.152.0
  model: gpt-5.4-codex  # Default model
  # Optional: custom endpoint
  # modelEndpointRef:
  #   name: openai-endpoint
```

**Runtime Features**:
- **Conformant**: No `experimental=true` flag required
- **CLI Version**: Uses official Rust CLI (rust-v0.152.0)
- **Model Support**: GPT-4, GPT-4o, GPT-5 variants
- **Wire Protocol**: Native OpenAI API compatibility

### 3. Role Definition

```yaml
# 05-role.yaml
apiVersion: ksquad.io/v1alpha1
kind: Role
metadata:
  name: codex-coder
  namespace: codex-squad
spec:
  name: Codex Coder
  description: "Software developer using OpenAI's Codex runtime"
  prompt: |
    You are an experienced software developer using OpenAI's Codex runtime.
    You write clean, efficient code with a focus on modern best practices.
    
    Your coding approach:
    1. Understand the requirements thoroughly before implementation
    2. Break down complex problems into manageable steps
    3. Write modular, maintainable code with clear documentation
    4. Consider error handling and edge cases
    5. Test your implementations thoroughly
    6. Follow the project's coding standards and conventions
    
    You are proficient in multiple programming languages and frameworks,
    and you choose the right tool for each specific task.
  defaultSkills:
    - name: development
    - name: testing
    - name: documentation
```

### 4. Agent Composition

```yaml
# 06-agent.yaml
apiVersion: ksquad.io/v1alpha1
kind: Agent
metadata:
  name: codex-coder
  namespace: codex-squad
spec:
  roleRef:
    name: codex-coder
  runtimeRef:
    name: codex-runtime
  credentialSecretRef:
    name: model-credentials
    key: token  # Matches the key in the Secret
  # Optional: runtime-specific settings
  model:
    temperature: 0.7
    maxTokens: 4000
```

## Network Requirements

Codex requires network access to OpenAI's API endpoints:

```yaml
# NetworkPolicy for OpenAI access
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: allow-openai-egress
  namespace: codex-squad
spec:
  podSelector: {}
  policyTypes:
  - Egress
  egress:
  - to:
    - namespaceSelector: {}
      podSelector: {}
  - to:
    - ipBlock:
        cidr: 0.0.0.0/0
        except:
        - 10.0.0.0/8      # Private networks
        - 172.16.0.0/12   # Private networks  
        - 192.168.0.0/16  # Private networks
    ports:
    - protocol: TCP
      port: 443
```

**Required External Access**:
- `api.openai.com:443` - OpenAI API endpoint
- Custom endpoints if using BYO OpenAI-compatible services

## Advanced Configuration

### Custom OpenAI Endpoints

For Azure OpenAI or self-hosted endpoints:

```yaml
# 02-runtime-custom.yaml
apiVersion: ksquad.io/v1alpha1
kind: AgentRuntime
metadata:
  name: codex-runtime-custom
  namespace: codex-squad
spec:
  type: codex
  cliVersion: rust-v0.152.0
  modelEndpointRef:
    name: azure-openai-endpoint

# Separate Secret for custom endpoint
apiVersion: v1
kind: Secret
metadata:
  name: azure-openai-endpoint
  namespace: codex-squad
stringData:
  base-url: "https://your-resource.openai.azure.com"
  api-key: "your-azure-api-key"
```

### Multiple Models

Configure different models for different use cases:

```yaml
# Different runtime for different agents
apiVersion: ksquad.io/v1alpha1
kind: AgentRuntime
metadata:
  name: codex-fast
  namespace: codex-squad
spec:
  type: codex
  cliVersion: rust-v0.152.0
  model: gpt-4o-mini  # Faster, cheaper model

apiVersion: ksquad.io/v1alpha1
kind: AgentRuntime
metadata:
  name: codex-advanced
  namespace: codex-squad
spec:
  type: codex
  cliVersion: rust-v0.152.0
  model: gpt-5.4-codex  # Most capable model
```

## Security Considerations

### Credential Management
- **Rotation**: Regularly rotate API keys
- **Least Privilege**: Use API keys with minimal required permissions
- **Secret Management**: Consider external secret managers for production
- **Audit Logging**: Monitor API usage and costs

### Network Security
- **Egress Controls**: Restrict network access to only necessary endpoints
- **Ingress Protection**: Ensure proper ingress controls for console access
- **Pod Security**: Use appropriate security contexts for Codex pods

### Data Privacy
- **Input Data**: Be aware that code prompts are sent to OpenAI
- **Output Data**: Review generated code before committing to repositories
- **PII**: Avoid processing personally identifiable information

## Troubleshooting

### Common Issues

| Issue | Symptoms | Solution |
|-------|----------|----------|
| **Authentication Failed** | Agent stuck in "Pending", 401 errors | Verify API key is valid and has correct format |
| **Network Timeout** | Agent unresponsive, timeout errors | Check NetworkPolicy allows egress to OpenAI |
| **Model Not Found** | "Model not supported" errors | Verify model name matches OpenAI's available models |
| **Rate Limited** | API 429 errors, slow responses | Implement retry logic or upgrade to higher tier |
| **Credential Injection** | Agent fails to start | Secret key name matches Agent's `credentialSecretRef.key` |

### Debug Commands

```bash
# Check agent status and events
kubectl -n codex-squad describe agent codex-coder

# Check runtime configuration
kubectl -n codex-squad get agentruntime codex-runtime -o yaml

# Verify credential injection
kubectl -n codex-squad get secret model-credentials -o yaml

# Check pod logs for runtime issues
kubectl -n codex-squad logs -f deployment/codex-coder

# Test network connectivity
kubectl -n codex-squad run curl-test --rm -it --image=curlimages/curl -- curl -I https://api.openai.com/v1/models
```

### Health Checks

```bash
# Verify team composition
kubectl -n codex-squad get team codex-squad -o wide

# Check runtime availability
kubectl -n codex-squad get agentruntime

# Monitor API usage (if monitoring is configured)
kubectl -n codex-squad get metrics --selector=app=codex
```

## Cost Management

### Monitoring Usage
```bash
# Track API calls if monitoring is enabled
kubectl -n codex-squad get prometheusrules -l monitoring.k8squad.io/metrics=api-calls

# Check cost allocation
kubectl -n codex-squad get costallocation -o wide
```

### Optimization Strategies
1. **Model Selection**: Use appropriate models for each task (gpt-4o for complex, gpt-4o-mini for simple)
2. **Prompt Engineering**: Optimize prompts to reduce token usage
3. **Batch Processing**: Group similar tasks to reduce API calls
4. **Caching**: Cache similar responses when appropriate

## Integration Patterns

### With GitOps Workflows
```yaml
# Use GitOps for agent configuration
apiVersion: ksquad.io/v1alpha1
kind: Agent
metadata:
  name: gitops-coder
  namespace: codex-squad
  annotations:
    argocd.argoproj.io/sync-options: ServerSideApply=true
spec:
  roleRef:
    name: codex-coder
  runtimeRef:
    name: codex-runtime
  credentialSecretRef:
    name: model-credentials
    key: token
```

### With CI/CD Pipelines
```yaml
# Agent can review PRs, run tests, etc.
apiVersion: ksquad.io/v1alpha1
kind: Role
metadata:
  name: ci-coder
  namespace: codex-squad
spec:
  name: CI Coder
  description: "Agent for CI/CD pipeline integration"
  prompt: |
    You are a CI/CD specialist who integrates with GitHub Actions.
    You can review PRs, run tests, and help with deployments.
    # ... CI-specific prompt
```

## Best Practices

### 1. Configuration Management
- Keep runtime configurations version controlled
- Use environment-specific configurations
- Document model selection rationale
- Regularly update CLI versions

### 2. Security
- Implement proper credential rotation
- Use appropriate NetworkPolicies
- Monitor API usage and costs
- Follow principle of least privilege

### 3. Performance
- Choose appropriate models for each task
- Implement proper retry logic for API calls
- Monitor and optimize token usage
- Consider caching strategies

### 4. Development Workflow
- Test with small prompts first
- Gradually increase complexity
- Validate generated code before use
- Document agent behavior and decision patterns

## Related Documentation

- [Supported Agent Runtimes](../../docs/supported-agents.md)
- [BYO Credentials Guide](../../docs/getting-started/getting-started-byo-cred.md)
- [Compose an Agent](../../docs/compose-an-agent.md)
- [Credential Rotation Runbook](../../docs/runbooks/credential-rotation.md)
- [Main KSquad Documentation](../../README.md)

## Community and Support

- GitHub Issues: [Report bugs and request features](https://github.com/K8squad/K8squad/issues)
- Discussions: [Join community discussions](https://github.com/K8squad/K8squad/discussions)
- Discord: [Join our community server](https://discord.gg/k8squad)
