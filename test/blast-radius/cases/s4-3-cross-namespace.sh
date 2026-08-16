#!/usr/bin/env bash
# S4-3 — cross-namespace isolation (§12.1 / NFR-SEC1 / Epic X.1).
#
# Anchor 1:1 (cross_ns_reachable): a Team-A pod cannot reach Team-B
# Services (network) or read Team-B Secrets (RBAC).
#
#   conformance: victim svc UNREACHABLE from the hostile pod AND
#                `can-i get secret -n team-b` answers "no".
#   mutation   : (net) drop team-b's default-deny + widen team-a egress →
#                victim svc MUST become reachable;
#                (rbac) bind hostile-run into a widened team-b Role →
#                `can-i` MUST flip to "yes". Either arm not flipping = no teeth.
set -u
BASE=$(cd "$(dirname "$0")/.." && pwd)
# shellcheck source=lib.sh
. "$BASE/lib.sh"

NS_A="team-a${S4_SUFFIX:-}"
NS_B="team-b${S4_SUFFIX:-}"
ARM="${S4_ARM:-conformance}"
HOSTILE=hostile-run
VICTIM="http://victim-api.${NS_B}.svc.cluster.local/"
SA="system:serviceaccount:${NS_A}:hostile-run"

can_i_read_secret() {
  # exit 0 iff the API server would LET the hostile SA read team-b secrets
  [ "$(kubectl auth can-i get secret -n "$NS_B" --as="$SA" 2>/dev/null)" = "yes" ]
}

if [ "$ARM" = "conformance" ]; then
  echo "S4-3 cross-namespace isolation (conformance)"
  probe_http "$NS_A" "$HOSTILE" "$VICTIM"
  victim_blocked=$?
  if can_i_read_secret; then
    bad "hostile-run SA can read team-b secrets — scoped RBAC did not hold"
    rbac_held=1
  else
    rbac_held=0
  fi
  expect_pass "S4-3" "conformance" \
    "team-b victim svc unreachable (exit=$victim_blocked) AND hostile SA denied on team-b secrets" \
    $(( victim_blocked != 0 ? 0 : 1 ))
  # record the RBAC leg separately so a silent RBAC hole can't hide behind a net PASS
  if [ "$rbac_held" -ne 0 ]; then
    record "S4-3" "conformance" "FAIL" "RBAC leg: hostile SA may read team-b secrets"
  fi
else
  echo "S4-3 cross-namespace isolation (mutation: netpol dropped + RoleBinding widened)"
  # net arm: open team-b's ingress and give team-a sandboxes explicit egress to team-b
  kubectl -n "$NS_B" delete networkpolicy default-deny-all --ignore-not-found
  cat <<EOF | kubectl apply -f -
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: s4-3-widen-egress-to-victim
  namespace: ${NS_A}
spec:
  podSelector:
    matchLabels:
      ksquad.io/component: run-sandbox
  policyTypes: [Egress]
  egress:
    - to:
        - namespaceSelector:
            matchLabels:
              ksquad.io/tenant: ${NS_B}
      ports:
        - { protocol: TCP, port: 80 }
EOF
  # rbac arm: the widened binding (mutations/s4-3-rbac-widening.yaml shape)
  apply_fixture "$BASE/mutations/s4-3-rbac-widening.yaml" >/dev/null
  sleep 3
  probe_http "$NS_A" "$HOSTILE" "$VICTIM"
  net_escape=$?
  if can_i_read_secret; then rbac_escape=0; else rbac_escape=1; fi
  combined=$(( net_escape != 0 || rbac_escape != 0 ? 1 : 0 ))
  [ "$net_escape" -eq 0 ] || bad "victim svc still unreachable after netpol mutation — no teeth (net)"
  [ "$rbac_escape" -eq 0 ] || bad "can-i still 'no' after RoleBinding widening — no teeth (RBAC)"
  expect_escape "S4-3" \
    "mutated guards let the hostile pod reach team-b's svc AND read its secrets" "$combined"
fi
