# Control-plane full-lockdown verification (ISI-3910)

Enforcing-CNI proof for the **opt-in** control-plane full-lockdown carve-outs
introduced in ISI-3910 (follow-up to ISI-3907).

## What it proves

The release namespace is the **control plane** and, per arch §12.2, is
high-trust — so the blanket release-namespace default-deny is **opt-in and OFF by
default** (`egress.networkPolicy.releaseNamespaceDefaultDeny`). When you *do* turn
it on, the chart also renders a set of carve-out NetworkPolicies
(`templates/networkpolicy-carveouts.yaml` + the apiserver hop in
`templates/egress.yaml`) that keep every control-plane component functional under
the deny.

This suite renders the **real chart policies** (not hand-written fixtures) into a
`kind` cluster running **Calico** (an enforcing CNI — kind's default `kindnet` is
not trustworthy for egress policy), stands up light `agnhost`/`busybox` pods
labelled exactly as the chart labels each component, and drives a connectivity
matrix in two arms.

### conformance arm (guards ON)

| case | path | expected |
|------|------|----------|
| C1 | operator → apiserver:8080 | reach (intra-CP allow) |
| C2 | operator → nats:4222 | reach (intra-CP allow) |
| C3 | outsider → console:3000 | reach (Gateway/Ingress dataplane ingress) |
| C4 | operator → outsider:8080 | **block** (egress lockdown teeth) |
| C5 | outsider → nats:4222 | **block** (non-carved cross-ns port) |

### mutation arm (teeth)

Delete `ksquad-cp-intra-namespace`, then re-probe C2. With the intra allow gone,
operator egress is confined to the kube-apiserver hop only, so
`operator → nats:4222` **must flip to BLOCK**. A mutation that does not flip means
the policy had no teeth and the suite fails — an all-green run against spineless
policies is itself a failure.

## Run it

```bash
# needs a kind cluster from kind-config.yaml (default CNI disabled)
kind create cluster --name cp-lockdown --config test/cp-lockdown/kind-config.yaml
test/cp-lockdown/run.sh all
```

`SKIP_CALICO=1` skips the Calico bootstrap (for an already-enforcing cluster);
`CHART=<path>` overrides the chart location. CI runs this via
`.github/workflows/cp-lockdown.yml` on every PR that touches the NetworkPolicy
templates, chart values, or this suite.
