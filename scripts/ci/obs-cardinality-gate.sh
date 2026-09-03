#!/usr/bin/env bash
# General cardinality CI gate (story 13.6 / ISI-3122; obs-plan §5.6, §11.1 gate 1).
#
# Asserts: every Prometheus *Vec metric label key declared in the Go source
# (pkg/, internal/, cmd/) is a member of the bounded-cardinality allowlist
# (internal/observability/cardinality_allowlist.go, MetricLabelAllowlist). Any
# out-of-allowlist label key fails the build — cardinality discipline is tested,
# not hoped for. Classic unbounded identifiers (run.id / work_item.id /
# principal.id / trace_id / …) get a louder, specific diagnostic.
#
# This is the GENERAL gate over all metrics. It is distinct from the
# buildbrowser-scoped NFR-OBS3 firewall (obs-nfr-obs3-firewall.sh), which guards
# a different invariant (buildbrowser read-volume must never become a billing
# axis).
#
# Usage (CI):    scripts/ci/obs-cardinality-gate.sh
# Usage (test):  scripts/ci/obs-cardinality-gate.sh --allowlist <go-file> --scan <file-or-dir>
#
# Exit 0 = gate passed.  Exit 1 = cardinality violation (or a label list that
# cannot be statically verified).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

ALLOWLIST_FILE="$REPO_ROOT/internal/observability/cardinality_allowlist.go"
SCAN_TARGETS=("$REPO_ROOT/pkg" "$REPO_ROOT/internal" "$REPO_ROOT/cmd")
SCAN_OVERRIDDEN=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --allowlist) ALLOWLIST_FILE="$2"; shift 2 ;;
    --scan)
      if [[ $SCAN_OVERRIDDEN -eq 0 ]]; then SCAN_TARGETS=(); SCAN_OVERRIDDEN=1; fi
      SCAN_TARGETS+=("$2"); shift 2 ;;
    *) echo "Unknown argument: $1" >&2; exit 1 ;;
  esac
done

echo "== Cardinality gate (obs-plan §5.6, story 13.6) =="
echo "   Allowlist: $ALLOWLIST_FILE"

if [[ ! -f "$ALLOWLIST_FILE" ]]; then
  echo "FAIL — allowlist file not found: $ALLOWLIST_FILE" >&2
  exit 1
fi

# --- Parse the allowed + forbidden key sets out of the Go allowlist file. ------
# Each var is a `[]string{ ... }` block of quoted keys; pull the quoted tokens
# from between the var's opening `{` and the first line that is just `}`.
extract_var() {
  # $1 = var name, $2 = file
  awk -v var="$1" '
    $0 ~ var" = \\[\\]string\\{" { grab=1; next }
    grab && /^\}/ { grab=0 }
    grab { print }
  ' "$2" | grep -oE '"[^"]+"' | tr -d '"'
}

mapfile -t ALLOWED < <(extract_var "MetricLabelAllowlist" "$ALLOWLIST_FILE")
mapfile -t FORBIDDEN < <(extract_var "MetricLabelForbidden" "$ALLOWLIST_FILE")

if [[ ${#ALLOWED[@]} -eq 0 ]]; then
  echo "FAIL — could not parse any keys from MetricLabelAllowlist in $ALLOWLIST_FILE" >&2
  exit 1
fi

is_allowed()   { local k; for k in "${ALLOWED[@]}";   do [[ "$k" == "$1" ]] && return 0; done; return 1; }
is_forbidden() { local k; for k in "${FORBIDDEN[@]}"; do [[ "$k" == "$1" ]] && return 0; done; return 1; }

echo "   Allowed keys: ${#ALLOWED[@]}    Scanning: ${SCAN_TARGETS[*]}"

# --- Collect Go source files (skip tests + vendor). ---------------------------
FILES=()
for t in "${SCAN_TARGETS[@]}"; do
  if [[ -f "$t" ]]; then
    FILES+=("$t")
  elif [[ -d "$t" ]]; then
    while IFS= read -r f; do FILES+=("$f"); done < <(
      find "$t" -type f -name '*.go' ! -name '*_test.go' -not -path '*/vendor/*'
    )
  fi
done

violations=0
verified=0

for f in "${FILES[@]}"; do
  # Guard: every *Vec constructor must carry an inline []string{...} label arg,
  # so the gate can see the keys. A variable/expression label list (New*Vec(...,
  # labelKeys)) defeats static cardinality analysis — fail closed. Compare the
  # count of *Vec constructors to the count of inline label literals in the file.
  vec_count=$({ grep -oE 'New(Counter|Gauge|Histogram|Summary)Vec[[:space:]]*\(' "$f" || true; } | wc -l | tr -d ' ')
  lit_count=$(perl -0777 -ne '
    my $n = 0;
    while (/New(?:Counter|Gauge|Histogram|Summary)Vec\s*\(.*?\[\]string\{[^}]*\}/sg) { $n++ }
    print $n;' "$f")
  if [[ "$vec_count" -gt "$lit_count" ]]; then
    violations=$((violations + 1))
    rel="${f#"$REPO_ROOT"/}"
    echo "  FAIL — $rel: a Prometheus *Vec metric uses a non-literal label list; inline []string{\"...\"} so the gate can verify cardinality ($vec_count constructor(s), $lit_count inline label literal(s))." >&2
  fi
  # Extract the label-name []string{...} that is the argument to each Prometheus
  # *Vec constructor. `.*?` (non-greedy, DOTALL) stops at the FIRST []string{...}
  # after the constructor — which is exactly the label-names arg, because the
  # Opts struct only ever carries Name/Help/Buckets, never a bare []string.
  while IFS= read -r labellist; do
    [[ -z "$labellist" ]] && continue
    verified=$((verified + 1))
    # Split on commas; each entry should be a quoted literal key.
    IFS=',' read -ra parts <<< "$labellist"
    for raw in "${parts[@]}"; do
      tok="$(echo "$raw" | tr -d '[:space:]')"
      [[ -z "$tok" ]] && continue
      if [[ "$tok" =~ ^\"([^\"]+)\"$ ]]; then
        key="${BASH_REMATCH[1]}"
        if ! is_allowed "$key"; then
          violations=$((violations + 1))
          rel="${f#"$REPO_ROOT"/}"
          if is_forbidden "$key"; then
            echo "  FAIL — $rel: metric label \"$key\" is a FORBIDDEN unbounded identifier." >&2
            echo "         It must ride as a resource attribute / exemplar / span attr, never a metric label (obs-plan §1.2/§5.6)." >&2
          else
            echo "  FAIL — $rel: metric label \"$key\" is not in the cardinality allowlist." >&2
            echo "         If it is a bounded enum/registry, add it to MetricLabelAllowlist (with a source note); otherwise drop it to an exemplar." >&2
          fi
        fi
      else
        # A non-literal label key (a variable/expression) defeats static
        # cardinality analysis — fail closed rather than wave it through.
        violations=$((violations + 1))
        rel="${f#"$REPO_ROOT"/}"
        echo "  FAIL — $rel: metric label list contains a non-literal key ($tok); inline string literals so the gate can verify cardinality." >&2
      fi
    done
  done < <(perl -0777 -ne '
    while (/New(?:Counter|Gauge|Histogram|Summary)Vec\s*\(.*?\[\]string\{([^}]*)\}/sg) {
      my $l = $1; $l =~ s/\n/ /g; print "$l\n";
    }' "$f")
done

echo "   Verified $verified metric label list(s)."

if [[ $violations -gt 0 ]]; then
  echo "" >&2
  echo "CARDINALITY GATE FAILED: $violations out-of-allowlist metric label(s)." >&2
  exit 1
fi

echo "ALL CHECKS PASSED"
