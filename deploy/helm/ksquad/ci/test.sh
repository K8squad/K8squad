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

echo "ALL CHECKS PASSED"
