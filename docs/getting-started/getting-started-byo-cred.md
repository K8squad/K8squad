# Getting Started: BYO Credentials (ChatGPT/OpenAI)

This guide walks you through setting up KSquad with your own (BYO) ChatGPT/OpenAI credentials using the Codex runtime. This pattern applies to any service-account based credential system.

## Overview

BYO credentials allow you to use your own API keys with KSquad, giving you full control over authentication while keeping your keys secure within Kubernetes Secrets.

## Supported BYO Runtimes

| Runtime | Credential Class | Environment Variable | Notes |
|---------|------------------|---------------------|-------|
| **codex** | `service-account` | `OPENAI_API_KEY` | OpenAI's official Rust coding agent |
| **opencode** | `service-account` | Provider-specific | Open-source, provider-agnostic coding agent |
| **openclaw** | `service-account` | `ANTHROPIC_API_KEY` | Anthropic CLI variant |
| **hermes** | `service-account` | Runtime-specific | Custom implementation |

Local model runners such as Ollama are not runtime *types*; route them through a
`ModelEndpoint` (see [Alternative BYO Endpoints](#alternative-byo-endpoints)), which
projects `OPENAI_BASE_URL` into the run.

## Prerequisites

- A Kubernetes cluster (1.31+)
- `kubectl` and `helm` installed
- Your own OpenAI API key (for Codex examples)

## Step 1: Install KSquad Operator

```bash
# Add Helm repository and install the operator
helm repo add ksquad https://charts.k8squad.io
helm repo update
helm install ksquad ksquad/k8squad \
  --namespace k8squad-system --create-namespace
```

Wait for the operator to be ready:

```bash
kubectl -n k8squad-system rollout status deploy/ksquad-operator
```

## Step 2: Create Namespace and Credentials

### Create a Namespace

```yaml
# 00-namespace.yaml
apiVersion: v1
kind: Namespace
metadata:
  name: byo-cred-squad
```

### Create Credentials Secret

For OpenAI/Codex, create a Secret with your API key:

```bash
kubectl -n byo-cred-squad create secret generic model-credentials \
  --from-literal=token=sk-your-openai-api-key \
  --dry-run=client -o yaml | kubectl apply -f -
```

Or as a YAML file (`01-credentials.yaml`):

```yaml
# 01-credentials.yaml
apiVersion: v1
kind: Secret
metadata:
  name: model-credentials
  namespace: byo-cred-squad
stringData:
  token: "sk-your-openai-api-key"
type: Opaque
```

**Security Notes**:
- Never commit real API keys to version control
- Use `kubectl create secret` from environment variables in production
- Consider using external secret management tools for production deployments

## Step 3: Create AgentRuntime

```yaml
# 02-runtime.yaml
apiVersion: ksquad.io/v1alpha1
kind: AgentRuntime
metadata:
  name: codex-runtime
  namespace: byo-cred-squad
spec:
  type: codex
  cliVersion: rust-v0.152.0
```

## Step 4: Create Role and Agent

### Define a Role

```yaml
# 04-role.yaml
apiVersion: ksquad.io/v1alpha1
kind: Role
metadata:
  name: coder-role
  namespace: byo-cred-squad
spec:
  name: Coder
  description: "Software developer focused on implementation"
  prompt: |
    You are an experienced software developer specializing in implementation.
    You write clean, maintainable code following best practices.
    Break down complex problems into manageable steps.
    Focus on writing working code that solves the stated problem.
    Ask clarifying questions when requirements are unclear.
```

### Create the Agent

```yaml
# 05-agent.yaml
apiVersion: ksquad.io/v1alpha1
kind: Agent
metadata:
  name: coder-agent
  namespace: byo-cred-squad
spec:
  roleRef:
    name: coder-role
  runtimeRef:
    name: codex-runtime
  credentialSecretRef:
    name: model-credentials
    key: token
```

## Step 5: Create Team

```yaml
# 06-team.yaml
apiVersion: ksquad.io/v1alpha1
kind: Team
metadata:
  name: byo-cred-squad
  namespace: byo-cred-squad
spec:
  namespace: byo-cred-squad
```

## Step 6: Apply All Manifests

Apply the manifests in order:

```bash
kubectl apply -f 00-namespace.yaml
kubectl apply -f 01-credentials.yaml
kubectl apply -f 02-runtime.yaml
kubectl apply -f 04-role.yaml
kubectl apply -f 05-agent.yaml
kubectl apply -f 06-team.yaml
```

## Step 7: Verify Deployment

Check that all resources are created and healthy:

```bash
kubectl -n byo-cred-squad get team,agents,roles,runtimes

# Watch for the team to become ready
kubectl -n byo-cred-squad get team byo-cred-squad -w

# Describe the agent to check status
kubectl -n byo-cred-squad describe agent coder-agent
```

You should see the team status show "Agents Composed: True" and the agent should be "Ready".

## Step 8: Create Your First Run

Open the KSquad console:

```bash
kubectl port-forward -n k8squad-system svc/ksquad-console 8080:80
# → http://localhost:8080
```

In the console:
1. Select the `byo-cred-squad` team
2. Create a new Run with a coding task
3. Watch as the agent uses your BYO OpenAI credentials

## Network Requirements for Codex

Codex needs network access to OpenAI's API. Ensure your cluster can reach:

```yaml
# NetworkPolicy for OpenAI access
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: allow-openai-egress
  namespace: byo-cred-squad
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
        - 10.0.0.0/8
        - 172.16.0.0/12
        - 192.168.0.0/16
    ports:
    - protocol: TCP
      port: 443
```

## Rotating BYO Credentials

When you need to rotate your API key:

```bash
# Update the secret with a new key
kubectl -n byo-cred-squad create secret generic model-credentials \
  --from-literal=token=sk-new-openai-api-key \
  --dry-run=client -o yaml | kubectl apply -f -
```

Agent pods will pick up the new key on the next Secret projection. Running tasks may need to be restarted to use the new credentials.

## Alternative BYO Endpoints

If you're using a custom OpenAI-compatible endpoint (like Azure OpenAI or a self-hosted model):

```yaml
# 02-runtime-with-custom-endpoint.yaml
apiVersion: ksquad.io/v1alpha1
kind: AgentRuntime
metadata:
  name: codex-runtime-custom
  namespace: byo-cred-squad
spec:
  type: codex
  cliVersion: rust-v0.152.0
  modelEndpointRef:
    name: openai-endpoint
```

With a corresponding custom endpoint Secret:

```bash
kubectl -n byo-cred-squad create secret generic openai-endpoint \
  --from-literal=base-url=https://your-custom-endpoint \
  --from-literal=apiKey=sk-your-api-key \
  --dry-run=client -o yaml | kubectl apply -f -
```

## Troubleshooting

### Common Issues

| Issue | Solution |
|-------|----------|
| Agent stuck in "Pending" state | Check credentials Secret has correct key format |
| Network timeout errors | Verify NetworkPolicy allows egress to API endpoints |
| Authentication failures | Confirm API key is valid and has necessary permissions |
| Model not found | Check model specification in AgentRuntime |

### Debug Commands

```bash
# Check credential injection
kubectl -n byo-cred-squad describe agent coder-agent | grep -A5 -B5 "Secret"

# Check pod logs for runtime errors
kubectl -n byo-cred-squad logs -f deployment/coder-agent

# Verify network connectivity
kubectl -n byo-cred-squad run curl-test --rm -it --image=curlimages/curl -- curl -I https://api.openai.com/v1/models
```

## Next Steps

- [Read about supported runtimes](../supported-agents.md)
- [Learn about composing agents](../compose-an-agent.md)
- [Explore the BMAD squad](../getting-started-bmad.md)
- [Check the credential rotation runbook](../runbooks/credential-rotation.md)
