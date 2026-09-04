#!/usr/bin/env bash
#
# Story B (ISI-3619) chart test lane for the OTelConfig CR.
#
# Proves, purely from `helm template`, that the opt-in observability.otel.*
# values render the OTelConfig CR the way Story A serves and Story C consumes:
#
#   B-AC1  empty        -> NO OTelConfig CR (in-cluster default preserved)
#   B-AC2  one-signal   -> single CR, only the configured signal, auth key
#                          defaults to "token", traces-only sampling
#   B-AC2  all-signals  -> traces + metrics + logs, ns/name auth form
#   B-AC3  structural   -> kubeconform validates every rendered CR against the
#                          committed CRD's OpenAPI schema (grpc endpoints carry
#                          no path; http endpoints are full URLs; sampling only
#                          on traces). kubeconform is skipped-with-reason when
#                          the binary is absent so a laptop run still asserts
#                          the render contract.
#
# Usage: config/helm/ci/otelconfig-test.sh   (run from the repo root)
set -euo pipefail

CHART_DIR="${CHART_DIR:-config/helm}"
CI_DIR="${CHART_DIR}/ci"
CRD_FILE="config/crd/bases/ksquad.io_otelconfigs.yaml"
HELM="${HELM:-helm}"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass() { printf '  \033[32mPASS\033[0m %s\n' "$1"; }
fail() { printf '  \033[31mFAIL\033[0m %s\n' "$1"; exit 1; }

render() { # render <values-file> -> only the otelconfig template
  "$HELM" template t "$CHART_DIR" -f "$1" -s templates/otelconfig.yaml 2>/dev/null || true
}

count_kind() { grep -c '^kind: OTelConfig' <<<"$1" || true; }

echo "== B-AC1: empty observability.otel -> no CR =="
OUT="$(render "$CI_DIR/otelconfig-empty-values.yaml")"
[ "$(count_kind "$OUT")" -eq 0 ] || fail "empty fixture emitted an OTelConfig CR"
pass "no CR rendered for empty observability.otel"

echo "== B-AC2: one signal -> single traces-only CR =="
OUT="$(render "$CI_DIR/otelconfig-one-signal-values.yaml")"
[ "$(count_kind "$OUT")" -eq 1 ] || fail "expected exactly one OTelConfig CR"
grep -q '^  traces:' <<<"$OUT" || fail "traces block missing"
grep -q '^  metrics:' <<<"$OUT" && fail "metrics block present but not configured"
grep -q '^  logs:'    <<<"$OUT" && fail "logs block present but not configured"
grep -q 'key: "token"' <<<"$OUT" || fail "auth key did not default to token (W2)"
grep -q 'type: probabilistic' <<<"$OUT" || fail "probabilistic sampling missing (W3)"
pass "single CR with only traces, defaulted auth key, probabilistic sampling"

echo "== B-AC2: all signals -> traces+metrics+logs, sampling only on traces =="
OUT="$(render "$CI_DIR/otelconfig-all-signals-values.yaml")"
[ "$(count_kind "$OUT")" -eq 1 ] || fail "expected exactly one OTelConfig CR"
for sig in traces metrics logs; do
  grep -q "^  ${sig}:" <<<"$OUT" || fail "${sig} block missing"
done
[ "$(grep -c 'sampling:' <<<"$OUT")" -eq 1 ] || fail "sampling must appear exactly once (traces only)"
# http protocol canonicalizes to http/protobuf (W1); grpc stays grpc.
grep -q 'protocol: "http/protobuf"' <<<"$OUT" || fail "http did not canonicalize to http/protobuf (W1)"
grep -q 'namespace: "monitoring"'   <<<"$OUT" || fail "namespace/name auth form not parsed"
pass "all three signals rendered; sampling confined to traces; http canonicalized"

echo "== B-AC3: structural validation of rendered CRs (kubeconform) =="
if ! command -v kubeconform >/dev/null 2>&1; then
  echo "  SKIP kubeconform not on PATH — render contract already asserted above."
  echo "ALL RENDER ASSERTIONS PASSED"
  exit 0
fi

# Convert the committed CRD into a kubeconform schema file. kubeconform resolves
# custom schemas via the -schema-location template below; the file name must be
# <kind>-<group>-<version>.json (kubeconform lowercases .ResourceKind) to match.
SCHEMA_DIR="$TMP/schemas"
mkdir -p "$SCHEMA_DIR"
python3 - "$CRD_FILE" "$SCHEMA_DIR" <<'PY'
import json, sys, yaml
crd_path, out_dir = sys.argv[1], sys.argv[2]
with open(crd_path) as f:
    crd = yaml.safe_load(f)
group = crd["spec"]["group"]
kind = crd["spec"]["names"]["kind"].lower()
for ver in crd["spec"]["versions"]:
    schema = ver["schema"]["openAPIV3Schema"]
    name = f"{kind}-{group}-{ver['name']}.json"
    with open(f"{out_dir}/{name}", "w") as out:
        json.dump(schema, out)
    print(f"  wrote schema {name}")
PY

validate() { # validate <values-file>
  local out; out="$(render "$1")"
  [ -n "$(count_kind "$out")" ] || return 0
  # -schema-location default keeps native k8s kinds working; the second entry
  # resolves the OTelConfig CRD schema we just generated.
  kubeconform -strict -summary \
    -schema-location default \
    -schema-location "$SCHEMA_DIR/{{ .ResourceKind }}-{{ .Group }}-{{ .ResourceAPIVersion }}.json" \
    <<<"$out"
}
validate "$CI_DIR/otelconfig-one-signal-values.yaml"  || fail "one-signal CR failed kubeconform"
validate "$CI_DIR/otelconfig-all-signals-values.yaml" || fail "all-signals CR failed kubeconform"
pass "rendered CRs validate against the OTelConfig CRD schema"

echo "ALL ASSERTIONS PASSED"
