#!/usr/bin/env bash
# Self-check for the KSquad chart (ISI-2149): lint, render every exposure mode,
# and assert the fail-fast guards actually fail. No cluster required.
set -euo pipefail
CHART="$(cd "$(dirname "$0")/.." && pwd)"
pass() { echo "  ok  — $1"; }
fail() { echo "  FAIL — $1"; exit 1; }

# A render that must SUCCEED, optionally grepping for an expected string.
render_ok() { # <desc> <grep-or-emptystring> <set-args...>
  local desc="$1" want="$2"; shift 2
  local out
  if ! out="$(helm template t "$CHART" "$@" 2>&1)"; then
    echo "$out"; fail "$desc (expected success)"
  fi
  if [[ -n "$want" ]] && ! grep -q -- "$want" <<<"$out"; then
    echo "$out"; fail "$desc (missing: $want)"
  fi
  pass "$desc"
}

# A render (of a single template) that must SUCCEED and NOT contain a string.
render_lacks() { # <desc> <template> <forbidden> <set-args...>
  local desc="$1" tmpl="$2" forbidden="$3"; shift 3
  local out
  if ! out="$(helm template t "$CHART" --show-only "$tmpl" "$@" 2>&1)"; then
    echo "$out"; fail "$desc (expected success)"
  fi
  if grep -q -- "$forbidden" <<<"$out"; then
    echo "$out"; fail "$desc (must NOT contain: $forbidden)"
  fi
  pass "$desc"
}

# A render that must FAIL with an expected message fragment.
render_fail() { # <desc> <expect-msg> <set-args...>
  local desc="$1" msg="$2"; shift 2
  local out
  if out="$(helm template t "$CHART" "$@" 2>&1)"; then
    echo "$out"; fail "$desc (expected failure but succeeded)"
  fi
  grep -q -- "$msg" <<<"$out" || { echo "$out"; fail "$desc (wrong message)"; }
  pass "$desc"
}

GW=(--set exposure.mode=gateway
    --set exposure.gateway.gatewayClassName=cilium
    --set exposure.gateway.listeners.https.certSecretName=tls
    --set exposure.hostnames.console=ksquad.example.com
    --set exposure.hostnames.apiserver=api.example.com
    --set storage.storageClassName=fast-ssd)

# Lint with a valid values set — the chart deliberately fails on empty defaults
# (that IS the fail-fast guard; asserted separately below).
echo "== helm lint =="
if helm lint "$CHART" "${GW[@]}" >/dev/null 2>&1; then pass "lint clean (valid values)"; else
  helm lint "$CHART" "${GW[@]}"; fail "lint"; fi

echo "== positive renders =="
render_ok "gateway: creates Gateway w/ gatewayClassName" 'gatewayClassName: "cilium"' "${GW[@]}"
render_ok "gateway: apiserver HTTPRoute disables SSE timeout" 'request: "0s"' "${GW[@]}"
render_ok "gateway: CNPG PVC uses values StorageClass" 'storageClass: "fast-ssd"' "${GW[@]}"
render_ok "gateway: workspace StorageClass handed to operator" 'workspace.storageClassName: "fast-ssd"' "${GW[@]}"
render_ok "ingress: renders Ingress + SSE annotations" 'proxy-buffering' \
  --set exposure.mode=ingress --set exposure.ingress.className=nginx \
  --set exposure.ingress.tls.secretName=tls \
  --set exposure.hostnames.console=a.example.com \
  --set exposure.hostnames.apiserver=b.example.com \
  --set storage.storageClassName=std
render_ok "clusterip: Services only, no Gateway" 'kind: Service' \
  --set exposure.mode=clusterip --set storage.storageClassName=std
# per-family override beats global
render_ok "postgres per-family StorageClass override" 'storageClass: "db-class"' \
  --set exposure.mode=clusterip --set storage.storageClassName=std \
  --set storage.postgres.storageClassName=db-class

echo "== fail-fast guards =="
render_fail "missing gatewayClassName fails" "gatewayClassName is REQUIRED" \
  --set exposure.mode=gateway \
  --set exposure.gateway.listeners.https.certSecretName=tls \
  --set exposure.hostnames.console=a --set exposure.hostnames.apiserver=b \
  --set storage.storageClassName=std
render_fail "missing storageClassName fails (no cluster default)" "never relies on the cluster-default" \
  --set exposure.mode=clusterip
render_fail "bad exposure.mode fails" "exposure.mode must be one of" \
  --set exposure.mode=bogus --set storage.storageClassName=std
render_fail "https listener without cert fails" "certSecretName is REQUIRED" \
  --set exposure.mode=gateway --set exposure.gateway.gatewayClassName=envoy \
  --set exposure.hostnames.console=a --set exposure.hostnames.apiserver=b \
  --set storage.storageClassName=std
render_fail "gateway with both listeners disabled fails (ISI-2286 F2)" "at least one exposure.gateway.listeners" \
  --set exposure.mode=gateway --set exposure.gateway.gatewayClassName=envoy \
  --set exposure.gateway.listeners.http.enabled=false \
  --set exposure.gateway.listeners.https.enabled=false \
  --set exposure.hostnames.console=a --set exposure.hostnames.apiserver=b \
  --set storage.storageClassName=std

# Reusable minimal-core value set (clusterip so exposure is out of the way).
CORE=(--set exposure.mode=clusterip --set storage.storageClassName=std)

echo "== NATS / JetStream event bus (ISI-2253) =="
render_ok "nats: JetStream enabled StatefulSet renders by default" 'jetstream {' "${CORE[@]}"
render_ok "nats: single-replica default" 'replicas: 1' "${CORE[@]}"
render_ok "nats: JetStream PVC uses values StorageClass (never cluster-default)" 'storageClassName: "std"' "${CORE[@]}"
render_ok "nats: JetStream PVC uses per-family StorageClass override" 'storageClassName: "nats-class"' \
  "${CORE[@]}" --set storage.nats.storageClassName=nats-class
render_ok "relay: apiserver outbox→NATS relay ConfigMap renders" 'event-relay' "${CORE[@]}"
render_ok "relay: NATS URL is release-derived" 'relay.natsUrl: "nats://t-ksquad-nats' "${CORE[@]}"
render_ok "relay: subject taxonomy prefix present" 'relay.subjectPrefix: "ksquad"' "${CORE[@]}"
render_ok "relay: decoupled from write path (NATS-down never blocks)" 'relay.blocksWritePath: "false"' "${CORE[@]}"
render_ok "relay: NATS never gates apiserver health" 'relay.natsHealthGatesApiserver: "false"' "${CORE[@]}"
# HA toggle — same pattern as CNPG storage.postgres.instances.
render_ok "nats HA: clustered JetStream renders routes" 'cluster {' \
  "${CORE[@]}" --set nats.ha.enabled=true --set nats.ha.replicas=3
render_ok "nats HA: replicas honored" 'replicas: 3' \
  "${CORE[@]}" --set nats.ha.enabled=true --set nats.ha.replicas=3
# Core still installs with the bus off — NATS-down never blocks (§17.4).
render_ok "nats disabled: core still renders (no StatefulSet)" 'kind: Service' \
  "${CORE[@]}" --set nats.enabled=false
render_ok "nats disabled: relay still renders + buffers in outbox" 'relay.busBundled: "false"' \
  "${CORE[@]}" --set nats.enabled=false

echo "== NATS fail-fast guards =="
render_fail "nats.enabled + missing NATS StorageClass fails (no cluster default)" "never relies on the cluster-default" \
  --set exposure.mode=clusterip
render_fail "nats HA with even replicas fails (RAFT quorum)" "must be ODD" \
  "${CORE[@]}" --set nats.ha.enabled=true --set nats.ha.replicas=4
render_fail "nats HA with <3 replicas fails (RAFT quorum)" "must be >= 3" \
  "${CORE[@]}" --set nats.ha.enabled=true --set nats.ha.replicas=1

echo "== access-mode schema (ISI-2252, §9.4) =="
render_ok "accessMode RWO (default) passes schema" 'workspace.accessMode: "ReadWriteOnce"' \
  "${CORE[@]}"
render_ok "accessMode RWX passes schema (valid enum, warned not rejected)" 'workspace.accessMode: "ReadWriteMany"' \
  "${CORE[@]}" --set storage.workspace.accessMode=ReadWriteMany
render_ok "accessMode RWOncePod passes schema" 'workspace.accessMode: "ReadWriteOncePod"' \
  "${CORE[@]}" --set storage.workspace.accessMode=ReadWriteOncePod
render_fail "invalid accessMode fails schema enum (no silent bad PVC)" "must be one of the following" \
  "${CORE[@]}" --set storage.workspace.accessMode=ReadWriteMnay

echo "== OTel collector retired from chart (ISI-4163/ISI-4164) =="
# The chart-of-record for the collector topology is deploy/otel/ (live
# otel-operator CRs; gateway Service otel-gateway-collector.observability).
# This chart must NEVER render a competing collector: wrong Service name,
# wrong namespace, two managers on one pipeline.
out="$(helm template t "$CHART" "${CORE[@]}" 2>&1)" || { echo "$out"; fail "render (collector retired)"; }
grep -q -- '-otel-collector' <<<"$out" && { echo "$out"; fail "collector(retired): no '-otel-collector' resources may render"; }
grep -q 'component: otel-collector' <<<"$out" && { echo "$out"; fail "collector(retired): no otel-collector components may render"; }
grep -q 'ksquad.io/egress-source' <<<"$out" && { echo "$out"; fail "collector(retired): no Helm bootstrap egress overlay may render"; }
pass "collector(retired): chart renders zero collector resources by default"
# Legacy sets must not resurrect anything (unknown values are simply unused).
out="$(helm template t "$CHART" "${CORE[@]}" --set observability.collector.enabled=true 2>&1)" \
  || { echo "$out"; fail "render (legacy collector set ignored)"; }
grep -q -- '-otel-collector' <<<"$out" && { echo "$out"; fail "collector(retired): observability.collector.enabled=true must be inert"; }
pass "collector(retired): observability.collector.enabled=true is inert"

# SLO rules survive, collector-independent (app-level ksquad_* SLOs, §9).
out="$(helm template t "$CHART" "${CORE[@]}" 2>&1)" \
  || { echo "$out"; fail "render (slo default off)"; }
grep -q 'kind: PrometheusRule' <<<"$out" \
  && { echo "$out"; fail "slo(default): PrometheusRule must be OFF by default"; }
pass "slo: PrometheusRule OFF by default"
render_ok "slo: PrometheusRule renders when enabled" 'kind: PrometheusRule' \
  "${CORE[@]}" --set observability.sloRules.enabled=true
render_ok "slo: rules alert on ksquad_* app metrics (collector-independent)" 'KSquadWarmClaimLatencyHigh' \
  "${CORE[@]}" --set observability.sloRules.enabled=true

echo "== operator RBAC for egress reconcile (ISI-3747) =="
render_ok "operator: can patch deployments (rollout annotation)" '"deployments"' "${CORE[@]}"

echo "== release-namespace default-deny posture (ISI-3907) =="
# DEFAULT: the control plane is high-trust (arch §12.2) and the release namespace
# is NOT a tenant Run-workload namespace, so the blanket default-deny must NOT
# render — otherwise any enforcing CNI starves operator/apiserver/console/gateway/
# NATS/event-relay/scm-webhook. egress.yaml renders empty by default.
out="$(helm template t "$CHART" "${CORE[@]}" 2>&1)" || { echo "$out"; fail "render (default egress posture)"; }
grep -q 'ksquad-default-deny' <<<"$out" && { echo "$out"; fail "egress(default): default-deny must NOT render (CP high-trust §12.2)"; }
grep -q 'ksquad-apiserver-egress' <<<"$out" && { echo "$out"; fail "egress(default): apiserver carve-out must NOT render without a deny"; }
pass "egress(default): no release-ns default-deny / no lone apiserver carve-out"

# apiserverEgress.enabled alone (no default-deny) still renders nothing — a lone
# egress allow policy would ITSELF over-restrict the high-trust apiserver.
out="$(helm template t "$CHART" "${CORE[@]}" --set egress.networkPolicy.apiserverEgress.enabled=true 2>&1)" \
  || { echo "$out"; fail "render (apiserverEgress-only)"; }
grep -q 'ksquad-apiserver-egress' <<<"$out" && { echo "$out"; fail "egress: apiserver carve-out must stay gated on releaseNamespaceDefaultDeny"; }
pass "egress: apiserver carve-out stays gated without default-deny"

# OPT-IN: enabling releaseNamespaceDefaultDeny renders the deny AND its carve-out.
render_ok "egress(opt-in): renders release-ns default-deny" 'name: ksquad-default-deny' \
  "${CORE[@]}" --set egress.networkPolicy.releaseNamespaceDefaultDeny=true
render_ok "egress(opt-in): renders apiserver carve-out over the deny" 'ksquad-apiserver-egress' \
  "${CORE[@]}" --set egress.networkPolicy.releaseNamespaceDefaultDeny=true

echo "== control-plane full-lockdown carve-outs (ISI-3910) =="
CARVE=templates/networkpolicy-carveouts.yaml
LOCKDOWN=(--set egress.networkPolicy.releaseNamespaceDefaultDeny=true)
# DEFAULT (high-trust CP): the carve-outs must render NOTHING — a lone allow set
# with no deny would itself over-restrict the CP, and the default posture must be
# completely unaffected (only the deny toggle activates lockdown). Grep the FULL
# render (an empty --show-only would make helm error "template not found").
out="$(helm template t "$CHART" "${CORE[@]}" 2>&1)" || { echo "$out"; fail "render (carveouts default)"; }
grep -q 'ksquad-cp-' <<<"$out" && { echo "$out"; fail "carveouts(default): must NOT render without the deny"; }
pass "carveouts(default): nothing renders without the deny"
# OPT-IN: the full carve-out set renders over the deny.
render_ok "carveouts(opt-in): intra-namespace allow renders" 'name: ksquad-cp-intra-namespace' \
  "${CORE[@]}" "${LOCKDOWN[@]}"
render_ok "carveouts(opt-in): kube-apiserver egress renders" 'name: ksquad-cp-kube-api-egress' \
  "${CORE[@]}" "${LOCKDOWN[@]}"
render_ok "carveouts(opt-in): Gateway/Ingress dataplane ingress renders" 'name: ksquad-cp-dataplane-ingress' \
  "${CORE[@]}" "${LOCKDOWN[@]}"
render_ok "carveouts(opt-in): Prometheus scrape ingress renders" 'name: ksquad-cp-monitoring-ingress' \
  "${CORE[@]}" "${LOCKDOWN[@]}"
# Intra-namespace allow carries DNS + same-namespace egress (the CP chatter path).
render_ok "carveouts(opt-in): intra allow carries same-namespace egress" 'kubernetes.io/metadata.name: default' \
  "${CORE[@]}" "${LOCKDOWN[@]}" --show-only "$CARVE"
# North/south ingress targets the externally-exposed console+apiserver+scm-webhook.
render_ok "carveouts(opt-in): dataplane ingress selects console/apiserver/scm-webhook" 'values: \[console, apiserver, scm-webhook\]' \
  "${CORE[@]}" "${LOCKDOWN[@]}" --show-only "$CARVE"
# The carve-out set is independently gate-able (defense-in-depth off-switch).
out="$(helm template t "$CHART" "${CORE[@]}" "${LOCKDOWN[@]}" \
  --set egress.networkPolicy.controlPlaneCarveOuts.enabled=false 2>&1)" \
  || { echo "$out"; fail "render (carveouts disabled)"; }
grep -q 'ksquad-cp-' <<<"$out" && { echo "$out"; fail "carveouts: controlPlaneCarveOuts.enabled=false must suppress them"; }
pass "carveouts: controlPlaneCarveOuts.enabled=false suppresses them"
# dataplaneNamespaceSelector is tunable (stricter lockdown to a named gateway ns).
render_ok "carveouts(opt-in): dataplaneNamespaceSelector is tunable" 'kubernetes.io/metadata.name: envoy-gateway-system' \
  "${CORE[@]}" "${LOCKDOWN[@]}" --show-only "$CARVE" \
  --set egress.networkPolicy.controlPlaneCarveOuts.dataplaneNamespaceSelector.matchLabels.kubernetes\\.io/metadata\\.name=envoy-gateway-system

echo "ALL CHECKS PASSED"
