#!/usr/bin/env bash
# Shared library for the S4 blast-radius suite (Story 14.4 / ISI-2245).
#
# Result vocabulary (mirrors the bench anchor blast-radius-check.py — the kind
# cases below are its 1:1 translation):
#   PASS — guard-on contains the escape attempt (conformance arm), or
#          guard-off lets it escape (mutation arm: the case has real teeth).
#   FAIL — an escape got through (conformance), or a mutation did NOT flip
#          the case RED (mutation arm) — an all-green suite against a
#          spineless fixture is itself a failure (AC4).
#   SKIP — the primitive under test is not deployable in kind yet; emitted
#          as ::notice:: and recorded in the skip ledger (AC8: self-skip-
#          with-reason, never a silent drop).
#
# Case scripts are child processes of run-s4.sh, so every record() also
# echoes a machine-readable "S4RESULT|..." line that the orchestrator
# captures into the combined report.

log()  { printf '%s\n' "$*"; }
ok()   { log "  [PASS] $*"; }
bad()  { log "  [FAIL] $*"; }

record() { # record <case> <arm> <result> <detail>
  echo "S4RESULT|$1|$2|$3|$4"
}

skip_case() { # skip_case <case> <arm> <reason>
  echo "::notice::S4 skip ($2 arm): $1 — $3"
  record "$1" "$2" "SKIP" "$3"
}

expect_pass() { # expect_pass <case> <arm> <description> <exit-code>
  if [ "$4" -eq 0 ]; then
    ok "$3"
    record "$1" "$2" "PASS" "$3"
  else
    bad "$3"
    record "$1" "$2" "FAIL" "$3"
  fi
}

expect_escape() { # expect_escape <case> <description> <exit-code>
  # Mutation arm: the attack under a mutated guard MUST succeed (escape
  # observed, exit 0). A non-zero here means the case cannot bite — no teeth.
  if [ "$3" -eq 0 ]; then
    ok "$2 (mutation flips RED — teeth present)"
    record "$1" "mutation" "PASS" "$2"
  else
    bad "$2 (mutation did NOT flip RED — no teeth: an all-green suite against a spineless fixture is itself a failure)"
    record "$1" "mutation" "FAIL" "$2"
  fi
}

wait_deploy() { # wait_deploy <namespace> <deployment> [timeout]
  kubectl -n "$1" rollout status deploy "$2" --timeout="${3:-180s}"
}

wait_job() { # wait_job <namespace> <job> [timeout]
  kubectl -n "$1" wait "job/$2" --for=condition=complete --timeout="${3:-120s}"
}

wait_pod_ready() { # wait_pod_ready <namespace> <pod> [timeout]
  kubectl -n "$1" wait --for=condition=Ready "pod/$2" --timeout="${3:-120s}"
}

# probe_http <namespace> <pod> <url> [timeout-seconds]
# Returns 0 iff the pod fetched the URL (busybox wget, single try, short timeout).
probe_http() {
  local ns="$1" pod="$2" url="$3" t="${4:-4}"
  kubectl -n "$ns" exec "$pod" -- \
    wget -q -T "$t" -t 1 -O /dev/null "$url"
}

# probe_tcp <namespace> <pod> <host> <port> [timeout-seconds]
# Returns 0 iff a TCP connect to host:port succeeded from inside the pod.
probe_tcp() {
  local ns="$1" pod="$2" host="$3" port="$4" t="${5:-4}"
  kubectl -n "$ns" exec "$pod" -- nc -z -w "$t" "$host" "$port"
}

# apply_fixture <file>
# Applies a fixture/mutation manifest, namespace-suffixed when running the
# mutation arm (S4_SUFFIX set by run-s4.sh) so the two arms never share objects.
apply_fixture() {
  local f="$1"
  if [ -n "${S4_SUFFIX:-}" ]; then
    sed -e "s/\bteam-a\b/team-a${S4_SUFFIX}/g" \
        -e "s/\bteam-b\b/team-b${S4_SUFFIX}/g" \
        -e "s/\bksquad-infra\b/ksquad-infra${S4_SUFFIX}/g" "$f" \
      | kubectl apply -f -
  else
    kubectl apply -f "$f"
  fi
}

pod_ip() { # pod_ip <namespace> <pod>
  kubectl -n "$1" get pod "$2" -o jsonpath='{.status.podIP}'
}

# deployable <grep-pattern> — non-zero exit when a matching deployed object
# exists anywhere in the cluster (used by the AC8 dependency gates).
deployable() {
  kubectl get deploy,sts,svc -A --no-headers 2>/dev/null | grep -E "$1"
}
