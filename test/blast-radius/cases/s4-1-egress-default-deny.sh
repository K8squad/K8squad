#!/usr/bin/env bash
# S4-1 — default-deny egress (§12.2 / NFR-SEC4 / Epic X.1).
#
# Anchor 1:1 (egress_reachable): a sandbox with no allowlist entry cannot
# reach an arbitrary endpoint; the allowlisted path (via the team egress
# proxy) still resolves.
#
#   conformance: arbitrary dst UNREACHABLE + allowlisted path REACHABLE
#   mutation   : delete the team-a egress guard policies → arbitrary dst
#                MUST become reachable (escape). If it does not, no teeth.
set -u
BASE=$(cd "$(dirname "$0")/.." && pwd)
# shellcheck source=lib.sh
. "$BASE/lib.sh"

NS_A="team-a${S4_SUFFIX:-}"
ARM="${S4_ARM:-conformance}"
HOSTILE=hostile-run
ARBITRARY_IP="${S4_ARBITRARY_EGRESS_IP:-1.1.1.1}"

if [ "$ARM" = "conformance" ]; then
  echo "S4-1 default-deny egress (conformance)"
  probe_tcp "$NS_A" "$HOSTILE" "$ARBITRARY_IP" 80
  arbitrary_blocked=$?
  probe_http "$NS_A" "$HOSTILE" "http://egress-proxy.${NS_A}.svc.cluster.local:8080/model"
  allowlisted_ok=$?
  if [ "$arbitrary_blocked" -ne 0 ] && [ "$allowlisted_ok" -eq 0 ]; then
    expect_pass "S4-1" "conformance" \
      "arbitrary dst ${ARBITRARY_IP}:80 unreachable AND allowlisted model path reachable via proxy" 0
  else
    [ "$arbitrary_blocked" -eq 0 ] && bad "arbitrary egress endpoint ${ARBITRARY_IP}:80 REACHED — default-deny is not containing (is the CNI enforcing NetworkPolicy? use test/blast-radius/kind-config.yaml + the Calico bootstrap in run-s4.sh)"
    [ "$allowlisted_ok" -ne 0 ] && bad "allowlisted path via egress-proxy unreachable — over-denial broke the model path"
    expect_pass "S4-1" "conformance" \
      "arbitrary dst probe exit=$arbitrary_blocked, allowlisted probe exit=$allowlisted_ok" 1
  fi
else
  echo "S4-1 default-deny egress (mutation: egress NetworkPolicy deleted)"
  kubectl -n "$NS_A" delete networkpolicy default-deny-all allow-dns allow-to-egress-proxy proxy-egress-to-infra --ignore-not-found
  sleep 3   # let the policy agent converge
  probe_tcp "$NS_A" "$HOSTILE" "$ARBITRARY_IP" 80
  expect_escape "S4-1" "with egress policies deleted, arbitrary ${ARBITRARY_IP}:80 is reachable" $?
fi
