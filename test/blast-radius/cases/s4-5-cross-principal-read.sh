#!/usr/bin/env bash
# S4-5 — cross-principal same-Team read-authZ (§9.4 / NFR-SEC5, B1/F7 / 8.7d + 15.8 overlap).
#
# Anchor 1:1 (build_read_status): principal B GETs A's Run build view
# (same Team, B ≠ owner) → 404 EXISTENCE-HIDING — never 403 (a 403 leaks
# that the Run exists, AC5). Positive control: owner A → 200; cross-Team → 404.
#
# DEPENDENCY GATE (AC8, story 14.4 table): the 8.7d BFF per-principal read
# gate is done as CODE, but a kind-deployable apiserver/console needs the
# Epic 9 install path. Until then this case self-skips-with-reason in both
# arms — the semantics stay pinned by the bench anchor, and the driver
# activates automatically once the apiserver is deployable.
set -u
BASE=$(cd "$(dirname "$0")/.." && pwd)
# shellcheck source=lib.sh
. "$BASE/lib.sh"

ARM="${S4_ARM:-conformance}"
REASON="8.7d BFF read gate not deployable in kind yet (needs the Epic 9 apiserver install); semantics pinned by blast-radius-check.py meanwhile"

discover_url() {
  if [ -n "${BLAST_RADIUS_APISERVER_URL:-}" ]; then
    echo "$BLAST_RADIUS_APISERVER_URL"
    return 0
  fi
  deployable 'apiserver|ksquad' | head -1 | awk '{print "http://"$2}' 2>/dev/null
}

URL=$(discover_url || true)
if [ -z "$URL" ] || [ "$URL" = "http://" ]; then
  skip_case "S4-5" "$ARM" "$REASON"
  exit 0
fi

NS_A="team-a${S4_SUFFIX:-}"
HOSTILE=hostile-run
RUN_A="${BLAST_RADIUS_RUN_ID:-fixture-run-a}"

http_status() { # http_status <principal-header> <path>
  kubectl -n "$NS_A" exec "$HOSTILE" -- \
    wget -S -q -T 4 -t 1 -O /dev/null \
      --header="X-Test-Principal: $1" "${URL}$2" 2>&1 \
    | grep -oE 'HTTP/1\.[01] [0-9]{3}' | tail -1 | awk '{print $2}'
}

if [ "$ARM" = "conformance" ]; then
  echo "S4-5 cross-principal read-authZ (conformance) — apiserver: $URL"
  owner=$(http_status "A" "/api/runs/${RUN_A}/build/tree")
  peer=$(http_status "B" "/api/runs/${RUN_A}/build/tree")
  cross=$(http_status "C" "/api/runs/${RUN_A}/build/tree")
  if [ "$owner" = "200" ] && [ "$peer" = "404" ] && [ "$cross" = "404" ]; then
    expect_pass "S4-5" "conformance" \
      "owner 200, same-Team peer 404 (existence-hiding), cross-Team 404" 0
  else
    [ "$peer" = "403" ] && bad "same-Team peer got 403 — existence LEAK (AC5: deny must be 404, never 403)"
    [ "$owner" != "200" ] && bad "owner read failed ($owner) — positive control broken"
    [ "$peer" != "404" ] && [ "$peer" != "403" ] && bad "same-Team peer status $peer (content leak if 200)"
    [ "$cross" != "404" ] && bad "cross-Team status $cross"
    expect_pass "S4-5" "conformance" "owner=$owner peer=$peer cross=$cross" 1
  fi
else
  echo "S4-5 cross-principal read-authZ (mutation: owningPrincipal check dropped)"
  # The named mutation (AC4) lives in the BFF: drop the owningPrincipal check.
  # Until a kind-deployable apiserver exists there is nothing to mutate in
  # this cluster — the mutation is equally dependency-gated.
  skip_case "S4-5" "mutation" "mutation target (apiserver BFF) not deployable in kind yet; $REASON"
fi
