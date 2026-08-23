# Egress Configuration

This document describes the egress configuration for KSquad, including default-deny policies, egress proxies, and Project-specific egress rules.

## Overview

KSquad implements a default-deny egress policy (story 4.6) that blocks all outbound traffic by default. Projects can specify their egress requirements via the `egressPolicyRef` field, which triggers the creation of allowlist NetworkPolicies.

## Architecture

### Default-Deny Baseline

All namespaces created by KSquad have a default-deny NetworkPolicy (`ksquad-default-deny`) that blocks all egress traffic. This provides a secure baseline where no outbound communication is possible unless explicitly allowed.

### Egress Proxy Pattern

The recommended pattern for allowing outbound traffic is through an egress proxy:

1. **Sandbox pods** can only reach their team's egress proxy on port 8080
2. **Egress proxy** can reach infrastructure services on port 443
3. **Infrastructure services** only accept traffic from labeled egress proxies

This pattern ensures:
- All outbound traffic is proxied and auditable
- NetworkPolicies remain simple and focused
- Security boundaries are clearly defined

## Configuration

### Global Egress Settings

```yaml
egress:
  enabled: true
  defaultEgressProxy:
    service: "egress-proxy"
    port: 8080
  networkPolicy:
    defaultDenyName: "ksquad-default-deny"
    projectEgressPrefix: "ksquad-egress"
```

### Project Configuration

Projects can specify egress requirements in their spec:

```yaml
apiVersion: ksquad.io/v1alpha1
kind: Project
metadata:
  name: my-project
  namespace: team-a
spec:
  repo:
    url: "https://github.com/myorg/myrepo"
  # Reference to an egress policy
  egressPolicyRef:
    name: "my-egress-policy"
```

## Network Policies

### Default-Deny Policy

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: ksquad-default-deny
  namespace: team-a
spec:
  policyTypes:
  - Egress
  - Ingress
  egress: []  # No egress allowed by default
  ingress: [] # No ingress allowed by default
  podSelector: {} # Applies to all pods
```

### Project Egress Policy

When a Project specifies `egressPolicyRef`, the Project controller creates an allowlist policy:

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: my-project-egress
  namespace: team-a
  labels:
    ksquad.io/project: my-project
    ksquad.io/egress: allowlist
spec:
  policyTypes:
  - Egress
  podSelector:
    matchLabels:
      ksquad.io/project: my-project
  egress:
  - # Allow DNS
    ports:
    - protocol: TCP
      port: 53
    - protocol: UDP
      port: 53
  - # Allow egress proxy
    to:
    - podSelector:
        matchLabels:
          ksquad.io/component: egress-proxy
          ksquad.io/team: team-a
    ports:
    - protocol: TCP
      port: 8080
```

## Security Considerations

1. **Principle of Least Privilege**: Default-deny ensures pods cannot accidentally expose data
2. **Explicit Allowlists**: Only explicitly allowed destinations are reachable
3. **Auditability**: All traffic goes through egress proxies where it can be logged
4. **Isolation**: Each team/Project's egress rules are isolated from others

## Migration

Existing Projects can be migrated by adding the `egressPolicyRef` field. The Project controller will automatically create the corresponding NetworkPolicies.

## Troubleshooting

### Pods cannot reach external services

1. Check if the Project has an `egressPolicyRef` specified
2. Verify the NetworkPolicy was created successfully
3. Check egress proxy logs for blocked connections
4. Ensure the egress proxy is running and accessible

### NetworkPolicy not created

1. Check the Project controller logs for errors
2. Verify the `egressPolicyRef` references a valid policy
3. Ensure the Project controller is running