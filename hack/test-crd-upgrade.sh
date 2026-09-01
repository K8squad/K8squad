#!/usr/bin/env bash
# ISI-3518 / ADR-0002 (Option B): prove the standalone k8squad-crds chart
# delivers CRD schema changes on `helm upgrade` INDEPENDENTLY of the
# control-plane chart, that user CRs survive upgrade + uninstall, and that the
# CRDs-first install ordering holds.
#
# Self-contained and runnable against ANY throwaway kube-context (kind in CI or
# a local cluster). No secrets / registry access — fork-safe on self-hosted.
#
# Covers ADR §7 acceptance criteria:
#   AC-4  install k8squad-crds at an OLD revision (roles CRD missing a field),
#         helm upgrade k8squad-crds to HEAD WITHOUT touching the CP chart, and
#         assert the new field is served + a user CR survives.
#   AC-5  helm uninstall k8squad-crds keep=true leaves CRDs+CRs; keep=false
#         removes CRDs. helm uninstall k8squad (CP) leaves CRDs/CRs intact.
#   AC-6  install ordering: k8squad-crds then k8squad succeeds (Toolchain CR
#         admitted); the reverse (CP first, no CRDs) fails fast.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
CRDS_CHART="${CRDS_CHART:-${ROOT}/config/helm-crds}"
CP_CHART="${CP_CHART:-${ROOT}/config/helm}"
CRDS_REL="${CRDS_REL:-k8squad-crds}"
CP_REL="${CP_REL:-k8squad}"
CP_NS="${CP_NS:-k8squad-system}"
CRDS_NS="${CRDS_NS:-k8squad-crds}"   # CRD chart's own release ns — SEPARATE from CP_NS so it never
                                     # pre-creates (unowned) the namespace the CP chart must own.
CR_NS="${CR_NS:-crd-upgrade-test-cr}"
CRD="roles.ksquad.io"
FIELD="runtimeClassHint"   # optional Role spec field "added in the upgrade"
FP="{.spec.versions[0].schema.openAPIV3Schema.properties.spec.properties.${FIELD}.type}"

WORK="$(mktemp -d)"
OLD_CRDS="${WORK}/old-crds-chart"

cleanup() {
  helm uninstall "$CP_REL"   -n "$CP_NS"   >/dev/null 2>&1 || true
  helm uninstall "$CRDS_REL" -n "$CRDS_NS" >/dev/null 2>&1 || true
  for f in "${ROOT}"/config/crd/bases/ksquad.io_*.yaml; do
    n="$(grep -m1 '^  name: ' "$f" | awk '{print $2}')"
    kubectl delete crd "$n" >/dev/null 2>&1 || true
  done
  kubectl delete ns "$CR_NS" "$CP_NS" "$CRDS_NS" >/dev/null 2>&1 || true
  rm -rf "$WORK"
}
trap cleanup EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }
ok()   { echo "PASS: $*"; }
crd_count() { kubectl get crd -l app.kubernetes.io/part-of=k8squad --no-headers 2>/dev/null | wc -l | tr -d ' '; }

# ---------------------------------------------------------------------------
# AC-6 (reverse): control-plane chart FIRST, on a cluster with no CRDs, with the
# toolchain catalog enabled → must fail fast because kind Toolchain is unknown.
# ---------------------------------------------------------------------------
echo "== AC-6 reverse: CP chart before CRDs must fail fast =="
if helm install "$CP_REL" "$CP_CHART" -n "$CP_NS" --create-namespace \
     --set controlPlane.enabled=false --set tools.defaultCatalog.enabled=true \
     --wait --timeout 60s >/tmp/cp-first.log 2>&1; then
  fail "CP chart installed WITHOUT the CRDs present — ordering contract not enforced"
fi
grep -qiE "no matches for kind|ensure CRDs|unable to (build|recognize)" /tmp/cp-first.log \
  || fail "CP-first failed but not for the expected missing-CRD reason (see /tmp/cp-first.log)"
helm uninstall "$CP_REL" -n "$CP_NS" >/dev/null 2>&1 || true
# --create-namespace above created CP_NS imperatively (no Helm ownership); drop it
# so the real CP install below can create+own its rendered Namespace cleanly.
kubectl delete ns "$CP_NS" --ignore-not-found >/dev/null 2>&1 || true
ok "CP-first install fails fast when CRDs are absent (expected)"

# ---------------------------------------------------------------------------
# Build an OLD k8squad-crds chart whose roles CRD lacks spec.$FIELD. Strip the
# field from the plain controller-gen base, re-wrap with the SAME helper the
# sync target uses, and swap it into a copy of the CRD chart.
# ---------------------------------------------------------------------------
echo "== build old k8squad-crds chart (roles CRD without spec.${FIELD}) =="
cp -r "$CRDS_CHART" "$OLD_CRDS"
STRIPPED="${WORK}/roles-stripped.yaml"
python3 - "${ROOT}/config/crd/bases/ksquad.io_roles.yaml" "$FIELD" "$STRIPPED" <<'PY'
import sys, yaml
src, field, dst = sys.argv[1], sys.argv[2], sys.argv[3]
d = yaml.safe_load(open(src))
props = d["spec"]["versions"][0]["schema"]["openAPIV3Schema"]["properties"]["spec"]["properties"]
if field not in props:
    sys.exit(f"precondition failed: {field} not in current Role schema — pick another field")
del props[field]
yaml.safe_dump(d, open(dst, "w"), sort_keys=False)
print(f"stripped spec.{field} from old Role base")
PY
bash "${ROOT}/hack/wrap-crd-template.sh" "$STRIPPED" > "${OLD_CRDS}/templates/ksquad.io_roles.yaml"

# ---------------------------------------------------------------------------
# AC-6 (forward) + AC-4 setup: install k8squad-crds (OLD) first, then the CP
# chart with the catalog enabled — Toolchain CR must be admitted.
# ---------------------------------------------------------------------------
echo "== AC-6 forward: install k8squad-crds (old) then CP chart =="
helm install "$CRDS_REL" "$OLD_CRDS" -n "$CRDS_NS" --create-namespace --wait --timeout 120s
[ "$(crd_count)" = "11" ] || fail "expected 11 CRDs after installing k8squad-crds, got $(crd_count)"
helm install "$CP_REL" "$CP_CHART" -n "$CP_NS" --create-namespace \
  --set controlPlane.enabled=false --set tools.defaultCatalog.enabled=true \
  --wait --timeout 120s
kubectl get toolchain -n "$CP_NS" kubectl >/dev/null 2>&1 \
  || fail "toolchain-default-catalog Toolchain CR was not admitted after CRDs-first install"
ok "CRDs-first ordering works; Toolchain CR admitted"

echo "== AC-4: assert OLD k8squad-crds does NOT serve spec.${FIELD} =="
[ -z "$(kubectl get crd "$CRD" -o jsonpath="$FP" 2>/dev/null || true)" ] \
  || fail "old CRD chart already serves spec.${FIELD}; test setup wrong"
ok "old k8squad-crds does not serve spec.${FIELD}"

echo "== create a survivor Role CR =="
kubectl create ns "$CR_NS" >/dev/null 2>&1 || true
cat <<EOF | kubectl apply -f -
apiVersion: ksquad.io/v1alpha1
kind: Role
metadata:
  name: survivor
  namespace: ${CR_NS}
spec:
  promptRef:
    name: survivor-prompt
EOF
kubectl get role survivor -n "$CR_NS" >/dev/null || fail "could not create survivor Role CR"
ok "survivor Role CR created"

# ---------------------------------------------------------------------------
# AC-4 core proof: upgrade ONLY the CRD chart to HEAD — the CP chart is left
# untouched — and assert the new field is served + the CR survives.
# ---------------------------------------------------------------------------
echo "== AC-4: helm upgrade k8squad-crds to HEAD (CP chart untouched) =="
CRDS_REV_BEFORE="$(helm status "$CP_REL" -n "$CP_NS" -o json | python3 -c 'import json,sys;print(json.load(sys.stdin)["version"])')"
helm upgrade "$CRDS_REL" "$CRDS_CHART" -n "$CRDS_NS" --wait --timeout 120s
CRDS_REV_AFTER="$(helm status "$CP_REL" -n "$CP_NS" -o json | python3 -c 'import json,sys;print(json.load(sys.stdin)["version"])')"
[ "$CRDS_REV_BEFORE" = "$CRDS_REV_AFTER" ] \
  || fail "CP release revision changed ($CRDS_REV_BEFORE->$CRDS_REV_AFTER) — CP chart was touched"
ok "CP chart release untouched by the CRD-chart upgrade (rev $CRDS_REV_AFTER)"

[ -n "$(kubectl get crd "$CRD" -o jsonpath="$FP" 2>/dev/null || true)" ] \
  || fail "helm upgrade k8squad-crds did NOT propagate spec.${FIELD} to the served schema"
ok "helm upgrade k8squad-crds propagated spec.${FIELD} independently of the CP chart"
kubectl get role survivor -n "$CR_NS" >/dev/null || fail "survivor Role CR lost across CRD upgrade"
ok "survivor Role CR survived the CRD upgrade"

policy="$(kubectl get crd "$CRD" -o jsonpath='{.metadata.annotations.helm\.sh/resource-policy}')"
[ "$policy" = "keep" ] || fail "CRD missing helm.sh/resource-policy: keep (got '${policy}')"
ok "CRD annotated helm.sh/resource-policy: keep"

# ---------------------------------------------------------------------------
# AC-5: uninstall the CONTROL-PLANE chart → CRDs + CRs untouched (it owns none).
# ---------------------------------------------------------------------------
echo "== AC-5: helm uninstall CP chart leaves CRDs + CRs intact =="
helm uninstall "$CP_REL" -n "$CP_NS" --wait --timeout 120s
[ "$(crd_count)" = "11" ] || fail "CP-chart uninstall changed CRD count to $(crd_count)"
kubectl get role survivor -n "$CR_NS" >/dev/null 2>&1 || fail "CP-chart uninstall deleted a user CR"
ok "CP-chart uninstall left all 11 CRDs and the survivor CR intact"

# ---------------------------------------------------------------------------
# AC-5: uninstall k8squad-crds with keep=true (default) → CRDs + CRs retained.
# ---------------------------------------------------------------------------
echo "== AC-5: helm uninstall k8squad-crds (keep=true) retains CRDs + CRs =="
helm uninstall "$CRDS_REL" -n "$CRDS_NS" --wait --timeout 120s
[ "$(crd_count)" = "11" ] || fail "keep=true uninstall removed CRDs (count now $(crd_count))"
kubectl get role survivor -n "$CR_NS" >/dev/null 2>&1 || fail "keep=true uninstall deleted a user CR"
ok "keep=true uninstall retained all 11 CRDs and the survivor CR"

# ---------------------------------------------------------------------------
# AC-5: reinstall then uninstall k8squad-crds with keep=false → CRDs removed.
# (survivor CR is cascade-deleted with its CRD — expected for keep=false.)
# ---------------------------------------------------------------------------
echo "== AC-5: helm uninstall k8squad-crds (keep=false) removes CRDs =="
helm upgrade --install "$CRDS_REL" "$CRDS_CHART" -n "$CRDS_NS" --set keep=false --wait --timeout 120s
helm uninstall "$CRDS_REL" -n "$CRDS_NS" --wait --timeout 120s
sleep 5  # allow API server to finalize CRD deletion
[ "$(crd_count)" = "0" ] || fail "keep=false uninstall left $(crd_count) CRDs (expected 0)"
ok "keep=false uninstall removed all CRDs"

echo ""
echo "ALL CHECKS PASSED (ADR-0002 §7 AC-4/5/6): k8squad-crds upgrades CRD schema"
echo "independently of the control plane; CRs survive; CRDs-first ordering holds;"
echo "keep policy governs uninstall."
