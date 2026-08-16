#!/usr/bin/env bash
# S4-2 — exfil-via-allowlisted-endpoint is NAMED, not hidden (§12.2 / F11).
#
# Anchor 1:1 (egress_audited): the allowlisted hole is not containment — it
# is an audited, attributable path. This case does NOT assert "no egress";
# it asserts the egress proxy AUDIT RECORD exists for the allowlisted call.
#
#   conformance: allowlisted call succeeds AND the proxy access log carries
#                an attributable record (src pod IP + URI); a direct
#                (non-proxy) call to the upstream is refused.
#   mutation   : bypass the proxy (direct sandbox→infra egress allow) → the
#                same upstream call succeeds with NO audit record — the hole
#                becomes silent. That MUST be observable, else no teeth.
set -u
BASE=$(cd "$(dirname "$0")/.." && pwd)
# shellcheck source=lib.sh
. "$BASE/lib.sh"

NS_A="team-a${S4_SUFFIX:-}"
NS_INFRA="ksquad-infra${S4_SUFFIX:-}"
ARM="${S4_ARM:-conformance}"
HOSTILE=hostile-run
MODEL_DIRECT="http://model-internal.${NS_INFRA}.svc.cluster.local:443/model"
MODEL_VIA_PROXY="http://egress-proxy.${NS_A}.svc.cluster.local:8080/model"

audit_lines() { # lines in the proxy access log attributing the hostile pod
  local ip
  ip=$(pod_ip "$NS_A" "$HOSTILE")
  kubectl -n "$NS_A" logs deploy/egress-proxy --tail=200 2>/dev/null | grep -F "$ip" | grep -F "/model" | wc -l
}

if [ "$ARM" = "conformance" ]; then
  echo "S4-2 exfil-via-allowlisted is audited (conformance)"
  before=$(audit_lines)
  probe_http "$NS_A" "$HOSTILE" "$MODEL_VIA_PROXY"
  call_ok=$?
  after=$(audit_lines)
  probe_http "$NS_A" "$HOSTILE" "$MODEL_DIRECT"
  direct_blocked=$?
  if [ "$call_ok" -eq 0 ] && [ "$after" -gt "$before" ] && [ "$direct_blocked" -ne 0 ]; then
    expect_pass "S4-2" "conformance" \
      "allowlisted call OK, proxy audit gained an attributable record ($before→$after), direct non-proxy path refused" 0
  else
    [ "$call_ok" -ne 0 ] && bad "allowlisted call through the proxy failed"
    [ "$after" -le "$before" ] && bad "proxy audit did NOT record the allowlisted call — the hole is silent (F11 violation)"
    [ "$direct_blocked" -eq 0 ] && bad "sandbox reached model.internal DIRECTLY — the proxy path is not exclusive"
    expect_pass "S4-2" "conformance" \
      "call exit=$call_ok, audit $before→$after, direct exit=$direct_blocked" 1
  fi
else
  echo "S4-2 exfil-via-allowlisted is audited (mutation: audit path bypassed)"
  # Widen the sandbox's egress straight to infra:443 and remove the proxy
  # carve-out — the allowlisted upstream stays reachable but off the record.
  kubectl -n "$NS_A" delete networkpolicy allow-to-egress-proxy --ignore-not-found
  cat <<EOF | kubectl apply -f -
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: s4-2-bypass-direct-to-infra
  namespace: ${NS_INFRA}
spec:
  podSelector: {}
  policyTypes: [Ingress]
  ingress:
    - from:
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: ${NS_A}
      ports:
        - { protocol: TCP, port: 443 }
EOF
  # also let the sandbox egress to infra (ingress allow above is not enough:
  # egress from the sandbox side is still default-denied)
  cat <<EOF | kubectl apply -f -
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: s4-2-bypass-egress-to-infra
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
              kubernetes.io/metadata.name: ${NS_INFRA}
      ports:
        - { protocol: TCP, port: 443 }
EOF
  sleep 3
  before=$(audit_lines)
  probe_http "$NS_A" "$HOSTILE" "$MODEL_DIRECT"
  direct_ok=$?
  after=$(audit_lines)
  if [ "$direct_ok" -eq 0 ] && [ "$after" -eq "$before" ]; then
    expect_escape "S4-2" \
      "bypassed proxy reaches model.internal with NO new audit record ($before→$after) — the hole went silent" 0
  else
    [ "$direct_ok" -ne 0 ] && bad "direct path still blocked — mutation ineffective"
    [ "$after" -ne "$before" ] && bad "audit still recorded the call — mutation ineffective"
    expect_escape "S4-2" "direct exit=$direct_ok, audit $before→$after" 1
  fi
fi
