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

echo "== OTel collector config split base+egress (ISI-3747, ADR-0008 M1(b)) =="
COL=templates/otel-collector.yaml
# Deployment loads TWO --config sources (base + egress overlay).
render_ok "collector: base --config source mounted" '\-\-config=/conf/base/collector.yaml' "${CORE[@]}"
render_ok "collector: egress --config overlay source mounted" '\-\-config=/conf/egress/egress.yaml' "${CORE[@]}"
# Egress volume is optional so first boot (before operator writes it) never wedges.
render_ok "collector: egress ConfigMap volume is optional" 'optional: true' "${CORE[@]}"
# Base config keeps the redaction/tail_sampling backstop IN GitOps (ADR-0008 §Why-b).
render_ok "collector: base retains redaction processor" 'transform/redaction' "${CORE[@]}"
render_ok "collector: base retains tail_sampling" 'tail_sampling' "${CORE[@]}"
# Base defaults to safe debug/prometheus sinks and carries NO vendor exporter.
render_ok "collector: base traces default to debug sink" 'exporters: \[debug\]' "${CORE[@]}"
render_lacks "collector: base has NO vendor exporter (no endpoint)" "$COL" '/vendor' "${CORE[@]}"
render_lacks "collector: no bootstrap egress ConfigMap when endpoint empty" "$COL" 'egress-source: helm-bootstrap' "${CORE[@]}"

# Bootstrap/no-operator fallback: setting the DEPRECATED endpoint renders the
# egress overlay ConfigMap that the operator will later own.
DT=(--set observability.export.otlp.endpoint=https://abc.live.dynatrace.com/api/v2/otlp
    --set observability.export.otlp.auth.secretName=dt-token)
render_ok "collector(bootstrap): renders egress overlay ConfigMap" 'ksquad.io/egress-source: helm-bootstrap' "${CORE[@]}" "${DT[@]}"
render_ok "collector(bootstrap): https endpoint → otlphttp/vendor exporter" 'otlphttp/vendor:' "${CORE[@]}" "${DT[@]}"
render_ok "collector(bootstrap): overlay routes traces to vendor" 'exporters: \[otlphttp/vendor\]' "${CORE[@]}" "${DT[@]}"
render_ok "collector(bootstrap): auth stays env-indirected (never in ConfigMap)" 'Authorization: "${env:KSQUAD_OTLP_AUTH}"' "${CORE[@]}" "${DT[@]}"
render_ok "collector(bootstrap): bare host:port endpoint → otlp/vendor (gRPC)" 'otlp/vendor:' \
  "${CORE[@]}" --set observability.export.otlp.endpoint=otel.example.com:4317

echo "== operator RBAC for egress reconcile (ISI-3747) =="
render_ok "operator: can patch deployments (rollout annotation)" '"deployments"' "${CORE[@]}"

# Optional: if a collector binary is present, prove the two --config sources
# actually deep-merge and route to the vendor with redaction still upstream.
# Skips cleanly in CI images without the binary (the render asserts above are the
# floor; the full running-collector/operator e2e is ISI-3724 + cluster).
echo "== collector confmap merge validate (optional) =="
if command -v otelcol-contrib >/dev/null 2>&1; then
  TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
  # Extract each ConfigMap's block-scalar body (4-space indented, terminated by
  # the YAML doc separator) and de-indent it into a standalone config file.
  helm template t "$CHART" "${CORE[@]}" "${DT[@]}" --show-only "$COL" \
    | awk '/collector.yaml: \|/{f=1;next} f&&/^---/{f=0} f{sub(/^    /,"");print}' >"$TMP/base.yaml"
  helm template t "$CHART" "${CORE[@]}" "${DT[@]}" --show-only "$COL" \
    | awk '/egress.yaml: \|/{f=1;next} f&&/^---/{f=0} f{sub(/^    /,"");print}' >"$TMP/egress.yaml"
  if otelcol-contrib validate --config "$TMP/base.yaml" --config "$TMP/egress.yaml" >/dev/null 2>&1; then
    pass "merged base+egress config validates + loads"
  else
    otelcol-contrib validate --config "$TMP/base.yaml" --config "$TMP/egress.yaml" || true
    fail "merged base+egress config failed validate"
  fi
else
  echo "  skip — otelcol-contrib not on PATH (render assertions cover the split)"
fi

echo "ALL CHECKS PASSED"
