# Compose an Agent

This guide explains how to compose agents in KSquad by combining Roles, Runtimes, Skills, and Credentials to create powerful AI team members.

## Overview

In KSquad, an Agent is the composition of several key components:

- **Role**: Defines the agent's responsibilities, behavior, and place in the team hierarchy
- **Runtime**: Specifies the underlying AI model/runtime (Claude Code, Codex, etc.)
- **Skills**: Capabilities the agent can use (tool access, specialized functions)
- **Credentials**: Authentication for the underlying AI service
- **Team**: The organizational context that brings agents together

## Agent Composition Components

### 1. Role - The Agent's Identity

A Role defines what the agent does, how it behaves, and where it fits in the team structure.

```yaml
apiVersion: ksquad.io/v1alpha1
kind: Role
metadata:
  name: senior-architect
  namespace: my-team
spec:
  name: Senior Architect
  description: "Lead technical design and architecture decisions"
  prompt: |
    You are a Senior Architect with 15+ years of experience in software design.
    You make strategic technical decisions that balance business needs with 
    long-term maintainability.
    
    Your approach:
    1. Understand the business context and constraints
    2. Analyze trade-offs between different architectural patterns
    3. Consider scalability, security, and operational implications
    4. Document decisions clearly with rationale
    5. Mentor junior team members through your thought process
    
    You are decisive but open to input, and you defend your recommendations 
    with clear, data-backed reasoning.
  defaultSkills:
    - name: system-design
    - name: cost-analysis
    - name: documentation
```

### 2. Runtime - The Agent's Engine

The Runtime specifies which AI model and runtime environment the agent uses.

```yaml
apiVersion: ksquad.io/v1alpha1
kind: AgentRuntime
metadata:
  name: claude-code-runtime
  namespace: my-team
spec:
  type: claude-code
  cliVersion: rust-v0.152.0
  model: claude-3-5-sonnet-20241022
```

### 3. Skills - The Agent's Capabilities

Skills extend an agent's capabilities with specific tools, knowledge, and functions.

```yaml
apiVersion: ksquad.io/v1alpha1
kind: Skill
metadata:
  name: system-design
  namespace: my-team
spec:
  name: System Design
  description: "Create system diagrams, architecture documentation, and design patterns"
  source:
    inline:
      # Skill implementation here
  permissions:
    - apiGroups: [""]
      resources: ["configmaps", "secrets"]
      verbs: ["get", "list", "watch"]
  requires:
    toolchains:
      - name: plantuml
        version: "1.2024.0"
```

### 4. Credentials - The Agent's Authentication

Credentials provide secure access to external services.

```yaml
# For Claude Code with human-seat (auto-refreshed)
apiVersion: v1
kind: Secret
metadata:
  name: claude-credentials
  namespace: my-team
  labels:
    ksquad.io/credential-class: human-seat
stringData:
  token: "sk-ant-api03-..."  # Will be auto-refreshed
  refreshToken: "your-refresh-token"
  expiresAt: "2024-12-31T23:59:59Z"

# For Codex with BYO API key
apiVersion: v1
kind: Secret
metadata:
  name: openai-credentials
  namespace: my-team
stringData:
  token: "sk-your-openai-api-key"
```

## Creating a Complete Agent

### Step 1: Define the Role

Start with the Role that defines the agent's purpose:

```yaml
# architect-role.yaml
apiVersion: ksquad.io/v1alpha1
kind: Role
metadata:
  name: architect-role
  namespace: my-team
spec:
  name: Architect
  description: "Owns technical design and build sequencing"
  prompt: |
    You are the Architect responsible for making technical decisions.
    You translate business requirements into technical implementations.
    
    Your responsibilities:
    - Analyze requirements and constraints
    - Design system architecture and component interactions
    - Create technical specifications and documentation
    - Evaluate technologies and frameworks
    - Consider scalability, maintainability, and security
    
    You provide clear, structured recommendations and can explain
    the rationale behind your decisions.
  defaultSkills:
    - name: system-design
    - name: code-review
    - name: documentation
```

### Step 2: Create the Runtime

Choose the appropriate runtime for your agent:

```yaml
# architect-runtime.yaml
apiVersion: ksquad.io/v1alpha1
kind: AgentRuntime
metadata:
  name: architect-runtime
  namespace: my-team
spec:
  type: claude-code
  cliVersion: rust-v0.152.0
  model: claude-3-5-sonnet-20241022
```

### Step 3: Prepare Credentials

Create the appropriate credentials:

```bash
# For Claude Code with auto-refresh
kubectl -n my-team create secret generic claude-credentials \
  --from-literal=token="sk-ant-api03-..." \
  --from-literal=refreshToken="your-refresh-token" \
  --from-literal=expiresAt="2024-12-31T23:59:59Z" \
  --dry-run=client -o yaml | kubectl apply -f -

# For Codex with BYO API key
kubectl -n my-team create secret generic openai-credentials \
  --from-literal=token="sk-your-openai-api-key" \
  --dry-run=client -o yaml | kubectl apply -f -
```

### Step 4: Define the Agent

Combine all components into an Agent:

```yaml
# architect-agent.yaml
apiVersion: ksquad.io/v1alpha1
kind: Agent
metadata:
  name: lead-architect
  namespace: my-team
spec:
  roleRef:
    name: architect-role
  runtimeRef:
    name: architect-runtime
  credentialSecretRef:
    name: claude-credentials
    key: token
  # Optional: override role defaults with specific skills
  skillRefs:
    - name: system-design
    - name: cloud-architecture
  # Optional: model-specific settings
  model:
    temperature: 0.7
    maxTokens: 4000
```

### Step 5: Create the Team

Organize agents into teams:

```yaml
# my-team.yaml
apiVersion: ksquad.io/v1alpha1
kind: Team
metadata:
  name: my-team
  namespace: my-team
spec:
  namespace: my-team
  description: "Advanced software development team"
```

## Advanced Composition Patterns

### Mixed Runtime Teams

Combine different runtimes for specialized roles:

```yaml
kind: Team
metadata:
  name: mixed-runtime-team
  namespace: mixed-team
spec:
  namespace: mixed-team
---
kind: Agent
metadata:
  name: claude-architect
  namespace: mixed-team
spec:
  roleRef:
    name: architect-role
  runtimeRef:
    name: claude-code-runtime
  credentialSecretRef:
    name: claude-credentials
    key: token
---
kind: Agent
metadata:
  name: codex-developer
  namespace: mixed-team
spec:
  roleRef:
    name: developer-role
  runtimeRef:
    name: codex-runtime
  credentialSecretRef:
    name: openai-credentials
    key: token
```

### Agent with Custom Skills

Add specialized skills to an agent:

```yaml
kind: Agent
metadata:
  name: fullstack-developer
  namespace: my-team
spec:
  roleRef:
    name: developer-role
  runtimeRef:
    name: claude-code-runtime
  credentialSecretRef:
    name: claude-credentials
    key: token
  skillRefs:
    - name: frontend-development
    - name: backend-development
    - name: database-design
    - name: deployment-automation
```

### Hierarchical Team Structure

Define reporting relationships through Role composition:

```yaml
kind: Role
metadata:
  name: engineering-manager
  namespace: my-team
spec:
  name: Engineering Manager
  description: "Manages engineering team and project delivery"
  prompt: |
    You are an Engineering Manager responsible for team leadership.
    You oversee multiple architects and developers.
    # ... manager prompt
---
kind: Role
metadata:
  name: senior-developer
  namespace: my-team
spec:
  name: Senior Developer
  description: "Senior developer reporting to engineering manager"
  prompt: |
    You are a Senior Developer working under an Engineering Manager.
    # ... developer prompt
```

## Best Practices

### 1. Role Design
- Define clear, specific responsibilities
- Write detailed prompts that guide behavior
- Consider the agent's place in the team hierarchy
- Balance autonomy with collaboration needs

### 2. Runtime Selection
- Choose runtimes that match your use case
- Consider credential management requirements
- Factor in cost and performance characteristics
- Test with your specific prompts and requirements

### 3. Skill Composition
- Start with default skills and add specialized ones
- Consider RBAC requirements for each skill
- Document skill capabilities and limitations
- Balance capability with security constraints

### 4. Credential Management
- Use appropriate credential classes (human-seat vs service-account)
- Implement proper rotation procedures
- Follow principle of least privilege
- Never commit credentials to version control

### 5. Team Organization
- Define clear team hierarchies
- Ensure role complementarity
- Consider communication patterns between agents
- Start small and scale as needed

## Troubleshooting

### Common Composition Issues

| Issue | Solution |
|-------|----------|
| Agent fails to compose | Check all references (role, runtime, credentials) exist |
| Runtime not recognized | Verify runtime type is supported and CLI version is valid |
| Credential injection fails | Check Secret format and credentialClass label |
| Skills not resolving | Verify skill sources and toolchain requirements |
| Team composition fails | Check namespace alignment and resource dependencies |

### Debug Commands

```bash
# Check agent composition status
kubectl -n <namespace> describe agent <agent-name>

# Verify runtime availability
kubectl -n <namespace> get agentruntime <runtime-name>

# Check credential injection
kubectl -n <namespace> get secret <credential-name> -o yaml

# Validate team composition
kubectl -n <namespace> describe team <team-name>
```

## Examples

See the following examples for inspiration:
- [Basic team composition](../examples/bmad-team/)
- [Codex-specific example](../examples/codex/)
- [Mixed runtime squad](../examples/mixed-runtime/)

## Next Steps

- [Learn about supported runtimes](./supported-agents.md)
- [Setup BYO credentials](./getting-started/getting-started-byo-cred.md)
- [Explore the BMAD squad](../getting-started-bmad.md)
- [Read about credential management](../runbooks/credential-rotation.md)
