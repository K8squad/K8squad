#!/usr/bin/env bash
# S4-6 — memory poisoning / provenance forgery + covert channel
# (§7.3 / NFR-SEC6, F5/F6 / Epic X.3 + the 10.4/12.4 covert-channel guards).
#
# Anchor 1:1 (memory_read + coordination_driveable_by_memory): a hostile Run
# cannot forge another principal's provenance; hostile-authored reads surface
# as UNTRUSTED with the hostile author's identity; and memory/room writes can
# never mutate a claim/handoff/state — coordination rides shared work-items +
# fencing only.
#
# DEPENDENCY GATE (AC8, story 14.4 table): the memory service is done as
# code, but kind-deployability waits on the Epic 9 install path. Self-skips
# with reason in both arms until then; the bench anchor pins semantics.
set -u
BASE=$(cd "$(dirname "$0")/.." && pwd)
# shellcheck source=lib.sh
. "$BASE/lib.sh"

ARM="${S4_ARM:-conformance}"
REASON="memory service not deployable in kind yet (needs the Epic 9 install); semantics pinned by blast-radius-check.py meanwhile"

URL="${BLAST_RADIUS_MEMORY_URL:-}"
if [ -z "$URL" ]; then
  found=$(deployable 'memory' | head -1 || true)
  [ -n "$found" ] && URL="http://$(echo "$found" | awk '{print $2}')"
fi
if [ -z "$URL" ]; then
  skip_case "S4-6" "$ARM" "$REASON"
  exit 0
fi

NS_A="team-a${S4_SUFFIX:-}"
HOSTILE=hostile-run

http_status() { # http_status <principal-header> <method> <path> [body]
  local extra=()
  if [ -n "${4:-}" ]; then
    extra=(--header="Content-Type: application/json" --post-data="$4")
  fi
  kubectl -n "$NS_A" exec "$HOSTILE" -- \
    wget -S -q -T 4 -t 1 -O - \
      --header="X-Test-Principal: $1" "${extra[@]}" "${URL}$2" 2>&1 \
    | grep -oE 'HTTP/1\.[01] [0-9]{3}' | tail -1 | awk '{print $2}'
}

if [ "$ARM" = "conformance" ]; then
  echo "S4-6 poisoning/forgery + covert channel (conformance) — memory: $URL"
  # 1) FORGERY: hostile Run writes a fact claiming authorship by victim-principal.
  st=$(http_status hostile-run POST /memory/v1/records \
    '{"claimed_author":"victim-principal","payload":"steer-the-coordinator"}')
  forged_accepted=$([ "$st" = "200" ] || [ "$st" = "201" ]; echo $?)
  # 2) READ-BACK: the record must surface with the STAMPED (hostile) author, untrusted.
  body=$(kubectl -n "$NS_A" exec "$HOSTILE" -- wget -q -T 4 -t 1 -O - \
    --header="X-Test-Principal: coordinator" "${URL}/memory/v1/records?author=victim-principal" 2>/dev/null || true)
  stamped_ok=$(echo "$body" | grep -q '"author": *"hostile-run"' \
               && echo "$body" | grep -qi '"trust": *"untrusted"'; echo $?)
  # 3) COVERT CHANNEL: memory/room state must expose no claim/handoff/state verbs.
  verbs=$(http_status hostile-run GET /memory/v1/claims)
  no_coord_semantics=$([ "$verbs" = "404" ]; echo $?)

  if [ "$forged_accepted" -ne 0 ] && [ "$stamped_ok" -eq 0 ] && [ "$no_coord_semantics" -eq 0 ]; then
    expect_pass "S4-6" "conformance" \
      "forgery rejected-or-restamped hostile+untrusted; no coordination verbs on the memory API" 0
  else
    [ "$forged_accepted" -eq 0 ] && bad "forged authorship ACCEPTED as victim-principal (§7.3 break)"
    [ "$stamped_ok" -ne 0 ] && bad "read-back did not surface the stamped hostile author as untrusted"
    [ "$no_coord_semantics" -ne 0 ] && bad "memory API exposes coordination semantics (claims/handoff) — off-record steering path (10.4/12.4)"
    expect_pass "S4-6" "conformance" \
      "forgery=$forged_accepted stamped=$stamped_ok covert=$no_coord_semantics" 1
  fi
else
  echo "S4-6 poisoning/forgery + covert channel (mutation: provenance stamp dropped)"
  # Named mutation (AC4): the memory service accepts the claimed author.
  # Dependency-gated exactly like the conformance arm.
  skip_case "S4-6" "mutation" "mutation target (memory service) not deployable in kind yet; $REASON"
fi
