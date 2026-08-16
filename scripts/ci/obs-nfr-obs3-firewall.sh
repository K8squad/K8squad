#!/usr/bin/env bash
# NFR-OBS3 Firewall Gate (ISI-2169 / ISI-2165 §7)
#
# Asserts: the §13 consumption/metering allowlist contains ZERO ksquad.buildbrowser.*
# series, AND no ksquad.buildbrowser.* instrument declares a "model" label.
#
# Usage (CI):    scripts/ci/obs-nfr-obs3-firewall.sh
# Usage (test):  scripts/ci/obs-nfr-obs3-firewall.sh --allowlist <path>
#
# Exit 0 = gate passed.  Exit 1 = NFR-OBS3 violation detected.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

pass() { printf "  ok  — %s\n" "$1"; }
fail() { printf "  FAIL — %s\n" "$1" >&2; exit 1; }

# ---------------------------------------------------------------------------
# Allowlist file resolution.
# Default: internal/observability/metering_allowlist.go
# Override with --allowlist <path> for the red-team test.
# ---------------------------------------------------------------------------
ALLOWLIST_FILE="$REPO_ROOT/internal/observability/metering_allowlist.go"
while [[ $# -gt 0 ]]; do
  case "$1" in
    --allowlist) ALLOWLIST_FILE="$2"; shift 2 ;;
    *) echo "Unknown argument: $1" >&2; exit 1 ;;
  esac
done

echo "== NFR-OBS3 Firewall Gate =="
echo "   Allowlist: $ALLOWLIST_FILE"

# ---------------------------------------------------------------------------
# Gate 1: metering allowlist must contain no ksquad.buildbrowser.* series.
# ---------------------------------------------------------------------------
if [[ ! -f "$ALLOWLIST_FILE" ]]; then
  # Allowlist not yet created — ISI-2168 OBS-BB5 pending.
  # Gate is vacuously green; it will enforce once the file exists.
  echo "  skip — allowlist file not found (ISI-2168 OBS-BB5 pending); gate will enforce once the file exists"
  echo "ALL CHECKS SKIPPED (pre-instrumentation)"
  exit 0
fi

if grep -E 'ksquad\.buildbrowser\.' "$ALLOWLIST_FILE" >/dev/null 2>&1; then
  echo "" >&2
  echo "NFR-OBS3 VIOLATION: ksquad.buildbrowser.* found in the metering allowlist." >&2
  echo "Build-read volume is legibility telemetry; it must NEVER be a consumption/billing axis." >&2
  echo "Offending lines:" >&2
  grep -n 'ksquad\.buildbrowser\.' "$ALLOWLIST_FILE" >&2
  fail "ksquad.buildbrowser.* in metering allowlist"
fi
pass "metering allowlist contains no ksquad.buildbrowser.* series"

# ---------------------------------------------------------------------------
# Gate 2: no ksquad.buildbrowser.* metric may declare a "model" label.
# The model axis is the billing disambiguator (arch §11); its absence on
# buildbrowser metrics is deliberate and load-bearing (plan §0.2).
# ---------------------------------------------------------------------------
if [[ -d "$REPO_ROOT/internal" ]]; then
  if grep -rE '"model"' "$REPO_ROOT/internal" \
       | grep -E 'buildbrowser' >/dev/null 2>&1; then
    echo "" >&2
    echo "NFR-OBS3 VIOLATION: ksquad.buildbrowser.* metric uses the 'model' label." >&2
    echo "The model label is the billing disambiguator; its presence on buildbrowser" >&2
    echo "metrics would make read volume accidentally aggregable into a consumption bill." >&2
    grep -rn '"model"' "$REPO_ROOT/internal" | grep 'buildbrowser' >&2
    fail "buildbrowser metric uses model label"
  fi
  pass "no ksquad.buildbrowser.* metric declares a model label"
else
  echo "  skip — internal/ not present (pre-instrumentation); label gate will enforce once source exists"
fi

echo "ALL CHECKS PASSED"
