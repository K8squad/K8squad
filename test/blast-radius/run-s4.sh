#!/usr/bin/env bash
# run-s4.sh — the S4 blast-radius suite orchestrator (Story 14.4 / ISI-2245).
#
# Runs the six S4 cases against a kind cluster in TWO arms:
#   conformance — guard ON: every escape attempt must be CONTAINED.
#   mutation    — guard mutated/deleted (AC4): every case must flip RED.
#                A mutation that does NOT flip = no teeth = failure.
#
# Dependency-gated cases (S4-5 apiserver / S4-6 memory, Epic 9 install)
# self-skip-with-reason into the visible skip ledger (AC8) — never a silent
# drop, and never a green the suite did not earn.
#
# Usage: test/blast-radius/run-s4.sh [conformance|mutation|all]
# Env:   BLAST_RADIUS_APISERVER_URL / BLAST_RADIUS_MEMORY_URL activate S4-5/6.
#        SKIP_CALICO=1 skips the Calico bootstrap (already-enforcing cluster).
set -euo pipefail
BASE=$(cd "$(dirname "$0")" && pwd)
# shellcheck source=lib.sh
. "$BASE/lib.sh"

ARM_ARG="${1:-all}"
CALICO_MANIFEST="https://raw.githubusercontent.com/projectcalico/calico/v3.28.2/manifests/calico.yaml"
RESULTS_FILE=$(mktemp /tmp/s4-results.XXXXXX)
LOG_DIR=$(mktemp -d /tmp/s4-logs.XXXXXX)
trap 'rm -f "$RESULTS_FILE"; rm -rf "$LOG_DIR"' EXIT

# ---------------------------------------------------------------- bootstrap --
# NetworkPolicy semantics REQUIRE an enforcing CNI. kind's default CNI
# (kindnet) is not trustworthy for egress policy, so the suite bootstraps
# Calico itself when absent (kind-config.yaml disables the default CNI).
ensure_cluster() {
  if [ "${SKIP_CALICO:-0}" != "1" ] \
     && ! kubectl -n kube-system get ds calico-node >/dev/null 2>&1; then
    log "bootstrap: applying Calico (${CALICO_MANIFEST})"
    kubectl apply -f "$CALICO_MANIFEST"
  fi
  if kubectl -n kube-system get ds calico-node >/dev/null 2>&1; then
    log "bootstrap: waiting for calico-node readiness (implies node readiness)"
    kubectl -n kube-system wait --for=condition=Ready pod -l k8s-app=calico-node --timeout=420s
  fi
  kubectl -n kube-system rollout status deploy coredns --timeout=180s
}

# ------------------------------------------------------------------- one arm --
run_arm() { # run_arm <arm-name> <suffix>
  local arm="$1" suffix="$2"
  export S4_SUFFIX="$suffix" S4_ARM="$arm"
  local ns_a="team-a${suffix}" ns_b="team-b${suffix}" ns_infra="ksquad-infra${suffix}"

  log ""
  log "====================================================================="
  log " S4 arm: $arm (namespaces: $ns_a / $ns_b / $ns_infra)"
  log "====================================================================="

  local f
  for f in "$BASE"/fixtures/*.yaml; do
    apply_fixture "$f" >/dev/null
  done

  log "waiting for fixture readiness…"
  wait_deploy "$ns_a" egress-proxy
  wait_deploy "$ns_infra" model-internal
  wait_deploy "$ns_infra" control-plane
  wait_deploy "$ns_b" victim-api
  wait_pod_ready "$ns_a" hostile-run

  local case_script log_file
  for case_script in "$BASE"/cases/*.sh; do
    log_file="$LOG_DIR/$(basename "$case_script" .sh).${arm}.log"
    log "---- $(basename "$case_script") [$arm]"
    set +e
    bash "$case_script" 2>&1 | tee "$log_file"
    set -e
    grep '^S4RESULT|' "$log_file" >>"$RESULTS_FILE" || true
  done
}

# -------------------------------------------------------------------- report --
write_final_report() {
  local failures skips total
  failures=$(awk -F'|' '$4=="FAIL"' "$RESULTS_FILE" | wc -l)
  skips=$(awk -F'|' '$4=="SKIP"' "$RESULTS_FILE" | wc -l)
  total=$(wc -l <"$RESULTS_FILE")

  {
    echo "S4 blast-radius suite — Story 14.4 / ISI-2245 — $(date -u +%FT%TZ)"
    echo
    printf '%-6s %-12s %-7s %s\n' "CASE" "ARM" "RESULT" "DETAIL"
    awk -F'|' '{printf "%-6s %-12s %-7s %s\n", $2, $3, $4, substr($0, index($0,$5))}' "$RESULTS_FILE"
    echo
    echo "total: $total   passed: $((total - failures - skips))   failed: $failures   skipped: $skips"
    echo
    if [ "$skips" -gt 0 ]; then
      echo "SKIP LEDGER (skipped-not-passed, AC8 — visible, never silent):"
      awk -F'|' '$4=="SKIP"{print "  - "$2" ("$3" arm): "substr($0, index($0,$5))}' "$RESULTS_FILE"
    else
      echo "SKIP LEDGER: empty."
    fi
  } | tee blast-radius-report.txt

  if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
    {
      echo "## Blast radius (S4 suite)"
      echo '```'
      cat blast-radius-report.txt
      echo '```'
    } >>"$GITHUB_STEP_SUMMARY"
  fi

  if [ "$failures" -gt 0 ]; then
    log ""
    log "RESULT: RED — $failures S4 invariant(s) violated (see report above)."
    exit 1
  fi
  log ""
  log "RESULT: GREEN — every ran case earned its pass; skips are itemized in the ledger."
  exit 0
}

# ---------------------------------------------------------------------- main --
ensure_cluster
case "$ARM_ARG" in
  conformance) run_arm conformance "" ;;
  mutation)    run_arm mutation "-mut" ;;
  all)
    run_arm conformance ""
    run_arm mutation "-mut"
    ;;
  *)
    echo "usage: $0 [conformance|mutation|all]" >&2
    exit 2
    ;;
esac
write_final_report
