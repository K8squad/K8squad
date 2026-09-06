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

## Credential test-connection probe egress (ISI-3891)

The E3-S2 test-connection endpoint (`POST /api/credentials/{name}/test`) dials a
BYO model endpoint **from the apiserver**, which runs in the control-plane
namespace with high-trust egress. To keep the probe from acting as a confused
deputy (an authenticated tenant probing control-plane-internal reachability by
status class), the probe applies the egress story's own rule at the dial seam:

**test-connection only reaches what a Run could reach.**

Concretely (`pkg/probeegress`): before dialing a BYO `endpointURL`, the
apiserver resolves the host and requires every resolved address (and the URL's
port) to fall inside the squad's DECLARED allowlist — the `EgressPolicy` CRs in
the caller's namespace plus any `EgressPolicy` a Project in the namespace
references via `spec.egressPolicyRef`. The same declaration drives the squad
NetworkPolicy story (§12.2 / story 4.6), so the probe and Runs cannot disagree.

Behavior squads will observe:

- **No EgressPolicy applies** → every BYO test-connection answers
  `ok: false` with a `Blocked — …` detail. This is the honest red: the squad's
  Runs could not reach the endpoint either, so a green would be a lie.
- **Endpoint outside the declared CIDRs** (or on a port the rules do not open)
  → same `Blocked` red.
- **Proxy-routed or namespaceSelector-only policies** → `Blocked`: the probe
  never rides the squad's egress proxy and cannot reproduce selector-based
  routes; declare the endpoint by CIDR to make it probeable.
- **Loopback, link-local (including `169.254.169.254`), unspecified and
  multicast targets are never probeable**, even when an allowlist entry covers
  them.
- Hostnames that do not resolve fail closed (a red, never a maybe).

The public provider probes (`api.anthropic.com`, `api.openai.com`) are pinned
constants, not caller-chosen, and are not affected.

The apiserver ServiceAccount needs read-only `egresspolicies` access for this
check (both charts grant `get`/`list`; the canonical chart's least-privilege
set is pinned by `TestApiserverClusterRoleLeastPrivilege`). A missing grant
surfaces as a `502 squad egress policy unavailable`, not a silent allow.



## Migration

Existing Projects can be migrated by adding the `egressPolicyRef` field. The Project controller will automatically create the corresponding NetworkPolicies.

## Troubleshooting

### Pods cannot reach external services

1. Check if the Project has an `egressPolicyRef` specified
2. Verify the NetworkPolicy was created successfully
3. Check egress proxy logs for blocked connections
4. Ensure the egress proxy is running and accessible

### BYO test-connection answers "Blocked"

1. Check that an `EgressPolicy` exists in the squad's namespace (or is
   referenced by a Project in it via `spec.egressPolicyRef`)
2. Verify a CIDR rule covers EVERY address the endpoint's hostname resolves
   to (a split multi-A answer is denied), on the port the URL uses
3. Loopback, link-local and metadata addresses are never probeable — point the
   endpoint Secret at a real address
4. The apiserver logs the precise denial server-side
   (`credential test BYO probe blocked`)

### The EgressPolicy to ship for BYO model endpoints (ISI-3906)

Public model providers (api.anthropic.com, api.openai.com,
generativelanguage.googleapis.com, BYO OpenAI-compatible gateways) publish
rotating A records, so a hostname-pinned CIDR allowlist rots. The workable
shape — the same governed-public-internet pattern the Helm chart uses for the
otel-collector vendor hop and the apiserver probe carve-out — is
**443/TCP to the public internet with the private/in-cluster/link-local
ranges carved out**:

```yaml
apiVersion: ksquad.io/v1alpha1
kind: EgressPolicy
metadata:
  name: model-providers
  namespace: <squad-namespace>   # the squad a Run (and the probe) dials from
spec:
  allow:
    - to:
        cidr: 0.0.0.0/0
        except:                  # never re-open control-plane internals
          - 10.0.0.0/8
          - 172.16.0.0/12
          - 192.168.0.0/16
          - 100.64.0.0/10       # RFC 6598 CGNAT (node/pod/LB ranges on GKE/EKS/Tailscale)
          - 169.254.0.0/16       # link-local incl. the cloud metadata service
      ports:
        - protocol: TCP
          port: 443
```

Why this is safe here:

- the probe-egress guard hard-blocks loopback, link-local, unspecified and
  multicast addresses **regardless of the allowlist**, so the carved-out
  ranges are belt-and-braces on top of the guard's own denials;
- the guard still requires **every** resolved address of the endpoint host to
  be covered, so an endpoint whose DNS folds in a private address is denied;
- tighten `except` to your cluster's actual pod/service/node CIDRs if those
  differ, and narrow `cidr` to provider ranges you can commit to maintaining
  if your posture requires it.

Reference it from the Project (or apply it directly in the squad namespace):

```yaml
apiVersion: ksquad.io/v1alpha1
kind: Project
metadata:
  name: my-project
  namespace: <squad-namespace>
spec:
  egressPolicyRef:
    name: model-providers
```

The console deliberately does NOT auto-create this policy: egress is a
declaration the squad's operator owns (§12.2 — "egress is policy, not
hardcode"), and a green test-connection on an endpoint the squad's Runs could
not reach would be a false green.

### The apiserver's own egress (chart)

The Helm chart's `egress.yaml` renders the `ksquad-default-deny` baseline
into the release namespace, which also selects the apiserver pod. Because the
control plane is architected for high-trust egress (§12.2), the chart ships a
carve-out policy (`ksquad-apiserver-egress`, gated by
`egress.networkPolicy.apiserverEgress.enabled`, default `true`) that opens:
DNS, in-namespace peers (Postgres), kube-system 443/6443 (the Kubernetes
API), and a governed external 443 hop with the private/link-local ranges
excepted (`egress.networkPolicy.apiserverEgress.exceptCIDRs`). Without this
carve-out, any CNI that enforces NetworkPolicy starves the test-connection
probe — every provider test answers
`Unreachable — the endpoint could not be reached (network error)`.


### NetworkPolicy not created

1. Check the Project controller logs for errors
2. Verify the `egressPolicyRef` references a valid policy
3. Ensure the Project controller is running