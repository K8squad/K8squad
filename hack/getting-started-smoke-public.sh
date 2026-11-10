#!/usr/bin/env bash
# getting-started-smoke-public.sh — fresh-install acceptance with PUBLIC default
# images (ISI-7279, follow-up to ISI-7278 / ISI-7277).
#
# The sibling lane (getting-started-smoke.sh) proves the documented catalog
# quickstart; this lane proves what a BRAND-NEW PUBLIC ADOPTER gets when they
# enable the control plane with the chart's public defaults: every image is
# pulled from the public registry resolved by the chart (ghcr.io/k8squad,
# tag = chart appVersion) — nothing is built or loaded into the cluster, so
# tag/chart/image skew (the ISI-7277 class: a hook image predating
# --migrate-only) fails HERE instead of at an adopter's first install.
#
# Lane steps (against whatever kube-context is selected; CI points it at a
# throwaway kind cluster on a GitHub-hosted runner — a host with NO access to
# any internal registry, which is exactly the adopter's world):
#   0. The public apiserver image carries --migrate-only (docker run --help).
#   1. Ephemeral Postgres for the chart's documented dev DSN path.
#   2. helm install the standalone CRDs chart (quickstart step 1).
#   3. helm install the control-plane chart with PUBLIC DEFAULTS only:
#      controlPlane.enabled, modelConfig.default.model, dev database.dsn,
#      nats.persistence.storageClassName. NO registry/tag overrides.
#   4. Assert the ksquad-migrate pre-install hook gate passed (helm blocks the
#      release on it; a retained Job object is dumped and fails the lane).
#   5. Control plane rolls Ready (all Deployments + the NATS StatefulSet).
#   6. Record the pulled image digests as evidence.

set -euo pipefail

RELEASE="${RELEASE:-k8squad}"
CP_NS="${CP_NS:-k8squad-system}"
CHART="${CHART:-config/helm}"
CRDS_CHART="${CRDS_CHART:-config/helm-crds}"
CRDS_RELEASE="${CRDS_RELEASE:-k8squad-crds}"
CRDS_NS="${CRDS_NS:-k8squad-crds}"
DB_NS="${DB_NS:-k8squad-db}"
APP_TAG="${APP_TAG:-}"              # empty = chart appVersion (the adopter default)
DEFAULT_MODEL="${DEFAULT_MODEL:-claude-sonnet-4}"
STORAGE_CLASS="${STORAGE_CLASS:-standard}"
CRD_TIMEOUT="${CRD_TIMEOUT:-180s}"
INSTALL_TIMEOUT="${INSTALL_TIMEOUT:-15m}"
ROLL_TIMEOUT="${ROLL_TIMEOUT:-10m}"
REGISTRY="${REGISTRY:-ghcr.io/k8squad}"

fail() { echo "::error::$*" >&2; exit 1; }
note() { echo "::notice::$*"; }
group() { echo "::group::$*"; }
endgroup() { echo "::endgroup::"; }

# ------------------------------------------------------------------ preflight
for c in kubectl helm jq docker; do command -v "$c" >/dev/null || fail "$c not on PATH"; done
kubectl cluster-info >/dev/null 2>&1 || fail "no reachable cluster on the current kube-context"
[ -d "$CHART" ]  || fail "chart not found: $CHART (run from repo root)"
[ -d "$CRDS_CHART" ] || fail "CRD chart not found: $CRDS_CHART"

# Resolve the tag under test: explicit APP_TAG wins; the default is the chart's
# appVersion — exactly what an adopter gets with controlPlane.image.tag unset.
if [ -z "$APP_TAG" ]; then
  APP_TAG="$(helm show chart "$CHART" | sed -n 's/^appVersion:[[:space:]]*//p' | tr -d '"' | head -1)"
  [ -n "$APP_TAG" ] || fail "could not resolve appVersion from $CHART"
fi
APISERVER_IMAGE="$REGISTRY/ksquad-apiserver:$APP_TAG"
echo "public fresh-install acceptance: release=$RELEASE cp-ns=$CP_NS image-tag=$APP_TAG (chart default) storage=$STORAGE_CLASS"

kubectl get storageclass "$STORAGE_CLASS" >/dev/null 2>&1 \
  || fail "StorageClass $STORAGE_CLASS not found — set STORAGE_CLASS for this cluster"

# ============================================================ STEP 0 — public image carries the flag
group "STEP 0 — public apiserver image supports --migrate-only ($APISERVER_IMAGE)"
docker pull --quiet "$APISERVER_IMAGE" >/dev/null \
  || fail "public image pull failed: $APISERVER_IMAGE (tag not published? — build-images semver lane)"
if docker run --rm "$APISERVER_IMAGE" --help 2>&1 | tee /tmp/apiserver-help.txt | grep -F -- --migrate-only; then
  echo "flag proof: --migrate-only present in $APISERVER_IMAGE --help"
else
  grep -F -- --migrate-only /tmp/apiserver-help.txt \
    || fail "public apiserver image $APISERVER_IMAGE does NOT support --migrate-only (ISI-7277 skew class: image predates the hook)"
fi
docker image inspect "$APISERVER_IMAGE" --format 'image digest: {{join .RepoDigests ", "}}'
endgroup

# ============================================================ STEP 1 — ephemeral Postgres (dev DSN path)
group "STEP 1 — ephemeral Postgres in $DB_NS (chart's dev DSN path)"
kubectl apply -f - <<'DB'
apiVersion: v1
kind: Namespace
metadata:
  name: k8squad-db
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: postgres
  namespace: k8squad-db
  labels: {app: postgres}
spec:
  replicas: 1
  selector: {matchLabels: {app: postgres}}
  template:
    metadata: {labels: {app: postgres}}
    spec:
      containers:
      - name: postgres
        image: postgres:16-alpine
        env:
        - {name: POSTGRES_USER, value: k8squad}
        - {name: POSTGRES_PASSWORD, value: k8squad}
        - {name: POSTGRES_DB, value: k8squad}
        ports: [{containerPort: 5432}]
        readinessProbe:
          exec: {command: [pg_isready, -U, k8squad, -d, k8squad]}
          initialDelaySeconds: 5
          periodSeconds: 5
---
apiVersion: v1
kind: Service
metadata:
  name: postgres
  namespace: k8squad-db
spec:
  selector: {app: postgres}
  ports: [{port: 5432, targetPort: 5432}]
DB
kubectl wait --for=condition=Available --timeout=180s -n "$DB_NS" deploy/postgres \
  || fail "ephemeral Postgres did not become Available"
DSN="postgres://k8squad:k8squad@postgres.$DB_NS.svc.cluster.local:5432/k8squad?sslmode=disable"
endgroup

# ============================================================ STEP 2 — CRDs chart (quickstart step 1)
group "STEP 2 — helm install $CRDS_RELEASE (chart=$CRDS_CHART)"
helm upgrade --install "$CRDS_RELEASE" "$CRDS_CHART" \
  --namespace "$CRDS_NS" --create-namespace \
  --wait --timeout "$CRD_TIMEOUT"
mapfile -t CRDS < <(kubectl get crd -o name | grep 'ksquad.io$' || true)
[ "${#CRDS[@]}" -gt 0 ] || fail "no ksquad.io CRDs registered after helm install"
for crd in "${CRDS[@]}"; do
  kubectl wait --for=condition=Established --timeout="$CRD_TIMEOUT" "$crd" || fail "CRD not Established: $crd"
done
echo "Established ${#CRDS[@]} ksquad.io CRDs."
endgroup

# ============================================================ STEP 3 — control plane, PUBLIC defaults
group "STEP 3 — helm install $RELEASE with PUBLIC defaults (no registry overrides)"
helm template "$RELEASE" "$CHART" --namespace "$CP_NS" \
  --set controlPlane.enabled=true \
  --set "modelConfig.default.model=$DEFAULT_MODEL" \
  --set "controlPlane.database.dsn=$DSN" \
  --set "controlPlane.nats.persistence.storageClassName=$STORAGE_CLASS" \
  ${APP_TAG:+--set "controlPlane.image.tag=$APP_TAG"} \
  > /tmp/public-install.yaml
echo "rendered control-plane images:"
grep -E '^\s+image:\s' /tmp/public-install.yaml | awk '{print "  " $2}' | sort -u
# Skew guards: the acceptance-critical images must resolve to the public
# registry at the tag under test, and no internal registry may leak in.
for c in apiserver operator memory event-relay; do
  grep -q "$REGISTRY/ksquad-$c:$APP_TAG" /tmp/public-install.yaml \
    || fail "rendered manifests miss public default image for $c at $APP_TAG"
done
if grep -E '10\.0\.0\.13|registry\.observable\.internal' /tmp/public-install.yaml; then
  fail "internal registry reference leaked into the public-default render"
fi
# The ksquad-migrate pre-install hook runs during this install; helm --wait
# blocks the release on it, so a BackoffLimitExceeded hook aborts the install
# (the exact fresh-adopter failure from ISI-7277).
helm upgrade --install "$RELEASE" "$CHART" --namespace "$CP_NS" --create-namespace \
  --set controlPlane.enabled=true \
  --set "modelConfig.default.model=$DEFAULT_MODEL" \
  --set "controlPlane.database.dsn=$DSN" \
  --set "controlPlane.nats.persistence.storageClassName=$STORAGE_CLASS" \
  ${APP_TAG:+--set "controlPlane.image.tag=$APP_TAG"} \
  --wait --timeout "$INSTALL_TIMEOUT"
endgroup

# ============================================================ STEP 4 — migrate hook gate evidence
group "STEP 4 — ksquad-migrate pre-install hook gate"
# hook-delete-policy is before-hook-creation,hook-succeeded: a clean install
# deletes the Job, so reaching here means helm did NOT abort on the hook. A Job
# still present post-install can only be a retained failure — dump and fail.
if kubectl get job ksquad-migrate -n "$CP_NS" >/dev/null 2>&1; then
  kubectl logs -n "$CP_NS" job/ksquad-migrate || true
  kubectl describe job -n "$CP_NS" ksquad-migrate || true
  fail "ksquad-migrate Job still present post-install — hook did not complete cleanly"
fi
note "pre-install migrate hook completed: release deployed and no failed hook Job remains"
endgroup

# ============================================================ STEP 5 — control plane rolls Ready
group "STEP 5 — control plane rolls Ready"
kubectl wait --for=condition=Available --timeout="$ROLL_TIMEOUT" --all deploy -n "$CP_NS" \
  || { kubectl get pods -n "$CP_NS" -o wide; fail "control-plane Deployments did not become Available"; }
for sts in $(kubectl get sts -n "$CP_NS" -o name); do
  kubectl rollout status "$sts" -n "$CP_NS" --timeout="$ROLL_TIMEOUT" \
    || fail "StatefulSet rollout stuck: $sts"
done
endgroup

# ============================================================ STEP 6 — pulled image evidence
group "STEP 6 — pulled image digests (evidence)"
kubectl get pods -n "$CP_NS" \
  -o custom-columns='POD:.metadata.name,IMAGE:.spec.containers[*].image,DIGEST:.status.containerStatuses[*].imageID' \
  --no-headers
endgroup

echo "public fresh-install acceptance PASSED: public defaults install clean on a fresh cluster (flag proof + migrate hook + control plane Ready)."
