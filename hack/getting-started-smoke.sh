#!/usr/bin/env bash
# getting-started-smoke.sh — reproduce the published getting-started quickstart on a
# fresh cluster and assert the squad applies clean with NO dangling refs (ISI-3476,
# unblocks QA gate ISI-3290 AC-5).
#
# AC-5 requires the documented quickstart to reproduce on a clean cluster. This script
# runs the getting-started steps VERBATIM against whatever kube-context is selected
# (CI points it at a throwaway kind cluster) and fails closed the moment a documented
# step drifts from reality:
#
#   1. helm install the chart with the default toolchain catalog enabled   (docs step 1)
#   2. kubectl apply -f examples/bmad-team/squad.yaml                        (docs step 2)
#   3. assert every cross-reference in the applied squad resolves to a live object,
#      and every Skill.requires.toolchains entry resolves in the cluster catalog
#      ("catalog enabled" — no dangling refs).
#
# The ref/toolchain checks are driven off the LIVE cluster objects (kubectl -o json),
# not a hardcoded inventory, so adding an Agent/Role/Skill to the example is covered
# automatically and a broken ref fails the lane without touching this script.
#
# Reconcile-to-Ready of the running agents is credential- and image-gated (the example
# ships REPLACE_ME model tokens and the operator image is not published to the throwaway
# cluster), so that final leg is a documented skip-with-reason (STEP 5) rather than a
# silent pass — matching the repo's skip-with-reason convention (see e2e.yml).
set -euo pipefail

# ------------------------------------------------------------------ config (overridable)
RELEASE="${RELEASE:-k8squad}"
CP_NS="${CP_NS:-k8squad-system}"                 # control-plane / catalog namespace (chart default)
SQUAD_NS="${SQUAD_NS:-bmad-squad}"               # example squad namespace
CHART="${CHART:-config/helm}"                    # the CRD + toolchain-catalog chart
SQUAD="${SQUAD:-examples/bmad-team/squad.yaml}"  # the published single-file squad manifest
CRD_TIMEOUT="${CRD_TIMEOUT:-120s}"

fail() { echo "::error::$*" >&2; exit 1; }
note() { echo "::notice::$*"; }
group() { echo "::group::$*"; }
endgroup() { echo "::endgroup::"; }

# ------------------------------------------------------------------ preflight
command -v kubectl >/dev/null || fail "kubectl not on PATH"
command -v helm    >/dev/null || fail "helm not on PATH"
command -v jq      >/dev/null || fail "jq not on PATH"
kubectl cluster-info >/dev/null 2>&1 || fail "no reachable cluster on the current kube-context"
[ -d "$CHART" ]  || fail "chart not found: $CHART (run from repo root)"
[ -f "$SQUAD" ]  || fail "squad manifest not found: $SQUAD (run from repo root)"

echo "getting-started smoke: release=$RELEASE cp-ns=$CP_NS squad-ns=$SQUAD_NS chart=$CHART"

# ============================================================ STEP 1 — helm install (docs step 1)
group "STEP 1 — helm install $RELEASE (chart=$CHART, defaultCatalog.enabled=true)"
# `upgrade --install` so the lane is idempotent on a re-run against the same cluster;
# a fresh cluster takes the install path. This is the documented command with the
# documented catalog flag.
helm upgrade --install "$RELEASE" "$CHART" \
  --namespace "$CP_NS" --create-namespace \
  --set tools.defaultCatalog.enabled=true \
  --wait --timeout "$CRD_TIMEOUT"
endgroup

group "STEP 1b — wait for the ksquad.io CRDs to be Established"
# The chart installs CRDs as Helm `crds/`; block until the API server has them
# Established before applying CRs (avoids a race where apply races CRD registration).
mapfile -t CRDS < <(kubectl get crd -o name | grep 'ksquad.io$' || true)
[ "${#CRDS[@]}" -gt 0 ] || fail "no ksquad.io CRDs registered after helm install — chart drift"
for crd in "${CRDS[@]}"; do
  kubectl wait --for=condition=Established --timeout="$CRD_TIMEOUT" "$crd" \
    || fail "CRD not Established: $crd"
done
echo "Established ${#CRDS[@]} ksquad.io CRDs."
endgroup

# ============================================================ STEP 2 — toolchain catalog present
group "STEP 2 — assert the default toolchain catalog rendered into $CP_NS"
CATALOG_JSON="$(kubectl get toolchains -n "$CP_NS" -o json)"
CATALOG_COUNT="$(echo "$CATALOG_JSON" | jq '.items | length')"
[ "$CATALOG_COUNT" -gt 0 ] || fail "no Toolchain objects in $CP_NS — 'defaultCatalog.enabled=true' produced an empty catalog"
echo "catalog Toolchains in $CP_NS ($CATALOG_COUNT):"
echo "$CATALOG_JSON" | jq -r '.items[] | "  - \(.metadata.name): \([.spec.versions[].version] | join(", "))"'
endgroup

# ============================================================ STEP 3 — apply the squad (docs step 2)
group "STEP 3 — kubectl apply -f $SQUAD"
# Real server-side apply — catches any CR that drifts from the installed CRD OpenAPI
# schema (a documented example the API server rejects = the quickstart is broken).
kubectl apply -f "$SQUAD"
endgroup

# ============================================================ STEP 4 — no dangling refs
group "STEP 4 — assert every squad cross-reference resolves (no dangling refs)"
DANGLING=0
resolve() { # kind name ns  → records a failure if the object is absent
  local kind="$1" name="$2" ns="$3"
  if kubectl get "$kind" "$name" -n "$ns" >/dev/null 2>&1; then
    echo "  ok   $kind/$name"
  else
    echo "  MISS $kind/$name  (ns=$ns)"; DANGLING=$((DANGLING+1))
  fi
}

# Build the live-catalog map "name@version" set once for toolchain resolution.
CATALOG_SET="$(echo "$CATALOG_JSON" \
  | jq -r '.items[] as $t | $t.spec.versions[] | "\($t.metadata.name)@\(.version)"' | sort -u)"
resolve_toolchain() { # name@version
  if grep -qxF "$1" <<<"$CATALOG_SET"; then
    echo "  ok   toolchain/$1"
  else
    echo "  MISS toolchain/$1  (not in $CP_NS catalog)"; DANGLING=$((DANGLING+1))
  fi
}

echo "-- Agent → roleRef / runtimeRef / credentialSecretRef"
while IFS=$'\t' read -r a role rt sec; do
  # NB: qualify to roles.ksquad.io — the bare "role" short name resolves to
  # rbac.authorization.k8s.io/Role, which would false-MISS every ksquad Role.
  [ -n "$role" ] && resolve roles.ksquad.io "$role" "$SQUAD_NS"
  [ -n "$rt" ]   && resolve agentruntime "$rt" "$SQUAD_NS"
  [ -n "$sec" ]  && resolve secret "$sec" "$SQUAD_NS"
done < <(kubectl get agents -n "$SQUAD_NS" -o json \
  | jq -r '.items[] | [.metadata.name, (.spec.roleRef.name // ""), (.spec.runtimeRef.name // ""), (.spec.credentialSecretRef.name // "")] | @tsv')

echo "-- Role → promptRef (ConfigMap) / defaultSkills[] (Skill)"
while IFS=$'\t' read -r r prompt skills; do
  [ -n "$prompt" ] && resolve configmap "$prompt" "$SQUAD_NS"
  for s in $skills; do [ -n "$s" ] && resolve skill "$s" "$SQUAD_NS"; done
done < <(kubectl get roles.ksquad.io -n "$SQUAD_NS" -o json \
  | jq -r '.items[] | [.metadata.name, (.spec.promptRef.name // ""), ([.spec.defaultSkills[]?.name] | join(" "))] | @tsv')

echo "-- Skill → requires.toolchains[] (cluster catalog, name@version)"
while read -r tc; do
  [ -n "$tc" ] && resolve_toolchain "$tc"
done < <(kubectl get skills -n "$SQUAD_NS" -o json \
  | jq -r '.items[] | (.spec.requires.toolchains // [])[]' | sort -u)

echo "-- Team → projects[] (Project) / agents[] (Agent)"
while read -r p; do [ -n "$p" ] && resolve project "$p" "$SQUAD_NS"; done < <(
  kubectl get teams -n "$SQUAD_NS" -o json | jq -r '.items[] | (.spec.projects[]?.name)')
while read -r ag; do [ -n "$ag" ] && resolve agent "$ag" "$SQUAD_NS"; done < <(
  kubectl get teams -n "$SQUAD_NS" -o json | jq -r '.items[] | (.spec.agents[]?.name)')

[ "$DANGLING" -eq 0 ] || fail "$DANGLING dangling reference(s) in the applied squad — the quickstart does NOT reproduce clean"
echo "all squad cross-references resolve — no dangling refs."
endgroup

# ============================================================ STEP 5 — reconcile-to-Ready (gated)
group "STEP 5 — operator reconcile-to-Ready (credential/image gated)"
OPERATOR_READY="$(kubectl get deploy -n "$CP_NS" -l app.kubernetes.io/component=operator \
  -o jsonpath='{.items[*].status.availableReplicas}' 2>/dev/null || true)"
TOKEN="$(kubectl get secret model-credentials -n "$SQUAD_NS" \
  -o jsonpath='{.data.token}' 2>/dev/null | base64 -d 2>/dev/null || true)"
if [ -n "${OPERATOR_READY:-}" ] && [ "${OPERATOR_READY:-0}" != "0" ] && [ -n "$TOKEN" ] && [ "$TOKEN" != "REPLACE_ME" ]; then
  echo "operator present and real model credentials injected — asserting Team Ready"
  kubectl wait --for=condition=Ready --timeout=180s team/"$SQUAD_NS" -n "$SQUAD_NS" \
    || fail "Team did not reconcile to Ready"
  echo "Team reconciled to Ready."
else
  note "reconcile-to-Ready skipped-with-reason: needs the operator image (deploy/helm) on the cluster AND a real model token (example ships REPLACE_ME). The credit-free lane proves the documented install+apply path and full ref integrity; wire real creds + operator to assert Ready."
fi
endgroup

echo "getting-started smoke PASSED: quickstart reproduces clean on a fresh cluster (install + apply + catalog + no dangling refs)."
