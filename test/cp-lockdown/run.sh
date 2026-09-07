#!/usr/bin/env bash
# run.sh — control-plane full-lockdown verification on an enforcing CNI (ISI-3910).
#
# Proves that with the OPT-IN release-namespace default-deny turned ON
# (egress.networkPolicy.releaseNamespaceDefaultDeny=true), the chart's carve-out
# NetworkPolicies (templates/egress.yaml + templates/networkpolicy-carveouts.yaml)
# keep the whole control plane FUNCTIONAL — and that the guards have TEETH.
#
# It renders the REAL chart policies (not hand-written fixtures) into a test
# namespace on kind + Calico, stands up light agnhost/busybox pods labelled as
# each control-plane component, and drives a connectivity matrix in two arms:
#
#   conformance — guards ON: every "must reach" path works, every "must not"
#                 path is blocked.
#   mutation    — a guard deleted (AC teeth): the matching path must FLIP.
#                 A mutation that does not flip = no teeth = failure (an all-green
#                 suite against spineless policies is itself a failure).
#
# Usage: test/cp-lockdown/run.sh [conformance|mutation|all]
# Env:   SKIP_CALICO=1     skip the Calico bootstrap (already-enforcing cluster)
#        CHART=<path>      override chart path (default: repo deploy/helm/ksquad)
set -euo pipefail
BASE=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$BASE/../.." && pwd)
CHART="${CHART:-$REPO/deploy/helm/ksquad}"
ARM_ARG="${1:-all}"
CALICO_MANIFEST="https://raw.githubusercontent.com/projectcalico/calico/v3.28.2/manifests/calico.yaml"

NS_CP=cp-lockdown          # the control-plane (release) namespace under lockdown
NS_OUT=cp-outsider         # a namespace OUTSIDE the CP (tenant/dataplane/monitor)
RELEASE=cp                 # helm Release.Name → app.kubernetes.io/instance=cp
AGNHOST=registry.k8s.io/e2e-test-images/agnhost:2.47
BUSYBOX=busybox:1.36

RESULTS_FILE=$(mktemp /tmp/cp-lockdown.XXXXXX)
trap 'rm -f "$RESULTS_FILE"' EXIT

log()  { printf '%s\n' "$*"; }
record() { echo "R|$1|$2|$3|$4" >>"$RESULTS_FILE"; }   # R|case|arm|result|detail
ok()   { log "  [PASS] $*"; }
bad()  { log "  [FAIL] $*"; }

# ---------------------------------------------------------------- bootstrap --
ensure_cluster() {
  if [ "${SKIP_CALICO:-0}" != "1" ] \
     && ! kubectl -n kube-system get ds calico-node >/dev/null 2>&1; then
    log "bootstrap: applying Calico (${CALICO_MANIFEST})"
    kubectl apply -f "$CALICO_MANIFEST"
  fi
  if kubectl -n kube-system get ds calico-node >/dev/null 2>&1; then
    log "bootstrap: waiting for calico-node readiness"
    kubectl -n kube-system wait --for=condition=Ready pod -l k8s-app=calico-node --timeout=420s
  fi
  kubectl -n kube-system rollout status deploy coredns --timeout=180s
}

# ------------------------------------------------------------- chart policies --
# Render the REAL chart NetworkPolicies with lockdown ON and apply them into the
# CP namespace. This is what proves the SHIPPING policies work — not a fixture.
apply_chart_policies() {
  local tmpl
  for tmpl in templates/egress.yaml templates/networkpolicy-carveouts.yaml; do
    helm template "$RELEASE" "$CHART" \
      --namespace "$NS_CP" \
      --set exposure.mode=clusterip \
      --set storage.storageClassName=std \
      --set egress.networkPolicy.releaseNamespaceDefaultDeny=true \
      --show-only "$tmpl" \
      | kubectl apply -n "$NS_CP" -f -
  done
}

# ---------------------------------------------------------------- stand-ins --
# Light pods labelled EXACTLY as the chart labels each component, so the rendered
# podSelectors bind them. agnhost listeners answer TCP; busybox clients probe.
apply_standins() {
  kubectl create ns "$NS_CP" --dry-run=client -o yaml | kubectl apply -f -
  kubectl create ns "$NS_OUT" --dry-run=client -o yaml | kubectl apply -f -
  cat <<YAML | kubectl apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: apiserver
  namespace: $NS_CP
  labels: { app.kubernetes.io/instance: $RELEASE, app.kubernetes.io/component: apiserver }
spec:
  containers:
    - name: c
      image: $AGNHOST
      args: ["netexec", "--http-port=8080"]
---
apiVersion: v1
kind: Pod
metadata:
  name: console
  namespace: $NS_CP
  labels: { app.kubernetes.io/instance: $RELEASE, app.kubernetes.io/component: console }
spec:
  containers:
    - name: c
      image: $AGNHOST
      args: ["netexec", "--http-port=3000"]
---
apiVersion: v1
kind: Pod
metadata:
  name: nats
  namespace: $NS_CP
  labels: { app.kubernetes.io/instance: $RELEASE, app.kubernetes.io/component: nats }
spec:
  containers:
    - name: c
      image: $AGNHOST
      args: ["netexec", "--http-port=4222"]
---
apiVersion: v1
kind: Pod
metadata:
  name: operator
  namespace: $NS_CP
  labels: { app.kubernetes.io/instance: $RELEASE, app.kubernetes.io/component: operator }
spec:
  containers:
    - name: c
      image: $BUSYBOX
      command: ["sleep", "3600"]
---
apiVersion: v1
kind: Pod
metadata:
  name: outsider-listener
  namespace: $NS_OUT
  labels: { role: outsider-listener }
spec:
  containers:
    - name: c
      image: $AGNHOST
      args: ["netexec", "--http-port=8080"]
---
apiVersion: v1
kind: Pod
metadata:
  name: outsider-client
  namespace: $NS_OUT
  labels: { role: outsider-client }
spec:
  containers:
    - name: c
      image: $BUSYBOX
      command: ["sleep", "3600"]
YAML
  for p in apiserver console nats operator; do
    kubectl -n "$NS_CP" wait --for=condition=Ready "pod/$p" --timeout=120s
  done
  for p in outsider-listener outsider-client; do
    kubectl -n "$NS_OUT" wait --for=condition=Ready "pod/$p" --timeout=120s
  done
}

ip_of() { kubectl -n "$1" get pod "$2" -o jsonpath='{.status.podIP}'; }

# reach <client-ns> <client-pod> <target-ip> <port> — 0 iff TCP/HTTP reached.
reach() {
  kubectl -n "$1" exec "$2" -- wget -q -T 4 -t 1 -O /dev/null "http://$3:$4/hostname"
}

# expect_reach / expect_block record PASS/FAIL for the conformance arm.
expect_reach() { # <case> <desc> <client-ns> <client-pod> <ip> <port>
  if reach "$3" "$4" "$5" "$6"; then ok "$2"; record "$1" conformance PASS "$2"
  else bad "$2 (expected REACH, got BLOCK)"; record "$1" conformance FAIL "$2"; fi
}
expect_block() { # <case> <desc> <client-ns> <client-pod> <ip> <port>
  if reach "$3" "$4" "$5" "$6"; then bad "$2 (expected BLOCK, got REACH)"; record "$1" conformance FAIL "$2"
  else ok "$2"; record "$1" conformance PASS "$2"; fi
}

# ----------------------------------------------------------------- the matrix --
run_conformance() {
  log ""; log "== conformance arm (guards ON) =="
  local api nats console outl
  api=$(ip_of "$NS_CP" apiserver); nats=$(ip_of "$NS_CP" nats)
  console=$(ip_of "$NS_CP" console); outl=$(ip_of "$NS_OUT" outsider-listener)

  # Must-reach: intra-CP chatter under the default-deny.
  expect_reach C1 "operator → apiserver:8080 (intra-CP allow)"      "$NS_CP" operator "$api" 8080
  expect_reach C2 "operator → nats:4222 (intra-CP allow)"          "$NS_CP" operator "$nats" 4222
  # Must-reach: north/south ingress from the Gateway/Ingress dataplane.
  expect_reach C3 "outsider → console:3000 (dataplane ingress)"     "$NS_OUT" outsider-client "$console" 3000
  # Must-block: CP egress cannot leave the namespace to an arbitrary peer.
  expect_block C4 "operator → outsider:8080 (egress lockdown teeth)" "$NS_CP" operator "$outl" 8080
  # Must-block: a non-carved cross-namespace ingress port stays denied.
  expect_block C5 "outsider → nats:4222 (cross-ns deny)"            "$NS_OUT" outsider-client "$nats" 4222
}

run_mutation() {
  log ""; log "== mutation arm (delete cp-intra-namespace → must FLIP) =="
  kubectl -n "$NS_CP" delete networkpolicy ksquad-cp-intra-namespace --ignore-not-found >/dev/null
  # Calico applies policy deletions fast, but give the dataplane a beat.
  sleep 5
  local nats; nats=$(ip_of "$NS_CP" nats)
  # C2 was REACH under the guard; with the intra allow gone, operator egress is
  # confined to kube-api only → this MUST now block. If it still reaches, the
  # policy had no teeth.
  if reach "$NS_CP" operator "$nats" 4222; then
    bad "operator → nats:4222 still REACHES after deleting cp-intra-namespace (NO TEETH)"
    record M1 mutation FAIL "operator → nats:4222 did not flip"
  else
    ok "operator → nats:4222 blocked after deleting cp-intra-namespace (teeth present)"
    record M1 mutation PASS "operator → nats:4222 flipped RED"
  fi
}

# -------------------------------------------------------------------- report --
report() {
  local failures total
  failures=$(awk -F'|' '$4=="FAIL"' "$RESULTS_FILE" | wc -l)
  total=$(wc -l <"$RESULTS_FILE")
  {
    echo "control-plane full-lockdown suite — ISI-3910 — $(date -u +%FT%TZ)"
    echo
    printf '%-4s %-12s %-7s %s\n' "CASE" "ARM" "RESULT" "DETAIL"
    awk -F'|' '{printf "%-4s %-12s %-7s %s\n", $2, $3, $4, $5}' "$RESULTS_FILE"
    echo
    echo "total: $total   passed: $((total - failures))   failed: $failures"
  } | tee cp-lockdown-report.txt
  if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
    { echo "## Control-plane full-lockdown (ISI-3910)"; echo '```'; cat cp-lockdown-report.txt; echo '```'; } \
      >>"$GITHUB_STEP_SUMMARY"
  fi
  if [ "$failures" -gt 0 ]; then
    log ""; log "RESULT: RED — $failures invariant(s) violated."; exit 1
  fi
  log ""; log "RESULT: GREEN — CP functional under default-deny, guards have teeth."; exit 0
}

# ---------------------------------------------------------------------- main --
ensure_cluster
apply_standins
apply_chart_policies
sleep 5   # let Calico program the freshly-applied policies
case "$ARM_ARG" in
  conformance) run_conformance ;;
  mutation)    run_conformance; run_mutation ;;   # mutation needs the guarded baseline first
  all)         run_conformance; run_mutation ;;
  *) echo "usage: $0 [conformance|mutation|all]" >&2; exit 2 ;;
esac
report
