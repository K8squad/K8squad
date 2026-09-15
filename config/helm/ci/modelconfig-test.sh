#!/usr/bin/env bash
#
# Model-Per-Role S2 (ISI-4460, parent ISI-4430) chart test lane for the
# system-default ModelConfig CR.
#
# Proves, purely from `helm template`, the install contract the board directed
# (352a3ef2) and the story's acceptance criteria:
#
#   AC1  bare render (no override)            -> FAILS with the required-message.
#                                                The default tier is the floor of
#                                                model resolution; it must never
#                                                be empty, so install aborts.
#   AC2  --set modelConfig.default.model=X    -> renders ONE ModelConfig named
#                                                `default` in the operator ns with
#                                                spec.model: X. Optional fallback /
#                                                endpoint refs render when set.
#   AC3  structural                           -> kubeconform validates the rendered
#                                                CR against the committed ModelConfig
#                                                CRD's OpenAPI schema. Skipped-with-
#                                                reason when the binary is absent so
#                                                a laptop run still asserts the
#                                                render contract.
#
# Usage: config/helm/ci/modelconfig-test.sh   (run from the repo root)
set -euo pipefail

CHART_DIR="${CHART_DIR:-config/helm}"
CRD_FILE="config/crd/bases/ksquad.io_modelconfigs.yaml"
HELM="${HELM:-helm}"
TEMPLATE="templates/modelconfig.yaml"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass() { printf '  \033[32mPASS\033[0m %s\n' "$1"; }
fail() { printf '  \033[31mFAIL\033[0m %s\n' "$1"; exit 1; }

render() { # render <helm --set args...> -> only the modelconfig template (stdout)
  "$HELM" template t "$CHART_DIR" "$@" -s "$TEMPLATE" 2>/dev/null || true
}

count_kind() { grep -c '^kind: ModelConfig' <<<"$1" || true; }

echo "== AC1: bare render (no model) FAILS with the required-message =="
# Capture combined output; the render MUST exit non-zero and name the value.
if ERR="$("$HELM" template t "$CHART_DIR" -s "$TEMPLATE" 2>&1)"; then
  fail "bare render succeeded; expected install to abort on unset model"
fi
grep -q 'modelConfig.default.model is required' <<<"$ERR" \
  || fail "abort message did not mention modelConfig.default.model (got: $ERR)"
pass "bare render aborts with the operator-facing required message"

echo "== AC2: --set model renders one default ModelConfig CR =="
OUT="$(render --set modelConfig.default.model=claude-sonnet-4)"
[ "$(count_kind "$OUT")" -eq 1 ] || fail "expected exactly one ModelConfig CR"
grep -q '^  name: default$'            <<<"$OUT" || fail "CR is not named 'default'"
grep -q 'model: "claude-sonnet-4"'     <<<"$OUT" || fail "spec.model did not carry the set value"
grep -q '^  namespace: k8squad-system$' <<<"$OUT" || fail "CR not in the operator namespace"
grep -q '^  fallbackModel:'  <<<"$OUT" && fail "fallbackModel present but not configured"
grep -q '^  modelEndpointRef:' <<<"$OUT" && fail "modelEndpointRef present but not configured"
pass "single default ModelConfig in k8squad-system with the set model"

echo "== AC2: optional fallbackModel + modelEndpointRef render when set =="
OUT="$(render \
  --set modelConfig.default.model=claude-sonnet-4 \
  --set modelConfig.default.fallbackModel.model=claude-haiku-4 \
  --set modelConfig.default.fallbackModel.modelEndpointRef.name=byo-endpoint \
  --set modelConfig.default.modelEndpointRef.name=byo-endpoint \
  --set modelConfig.default.modelEndpointRef.key=endpointURL)"
grep -q 'model: "claude-haiku-4"'  <<<"$OUT" || fail "fallbackModel.model missing"
grep -q 'name: "byo-endpoint"'     <<<"$OUT" || fail "modelEndpointRef.name missing"
grep -q 'key: "endpointURL"'       <<<"$OUT" || fail "modelEndpointRef.key missing"
pass "optional fallback + endpoint refs render under the default tier"

echo "== AC3: structural validation of the rendered CR (kubeconform) =="
if ! command -v kubeconform >/dev/null 2>&1; then
  echo "  SKIP kubeconform not on PATH — render contract already asserted above."
  echo "ALL RENDER ASSERTIONS PASSED"
  exit 0
fi

# Convert the committed CRD into a kubeconform schema file. The file name must be
# <kind>-<group>-<version>.json (kubeconform lowercases .ResourceKind) to match
# the -schema-location template below.
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

OUT="$(render --set modelConfig.default.model=claude-sonnet-4)"
kubeconform -strict -summary \
  -schema-location default \
  -schema-location "$SCHEMA_DIR/{{ .ResourceKind }}-{{ .Group }}-{{ .ResourceAPIVersion }}.json" \
  <<<"$OUT" || fail "rendered ModelConfig CR failed kubeconform"
pass "rendered CR validates against the ModelConfig CRD schema"

echo "ALL ASSERTIONS PASSED"
