#!/usr/bin/env bash
# public-images-smoke.sh — install the PUBLISHED control-plane chart with the
# PUBLIC default images on a fresh cluster and assert the install completes
# (ISI-7277 lane).
#
# Why this lane exists: every other install lane (getting-started-smoke, e2e)
# either leaves controlPlane.enabled=false or overrides images to
# built-from-source/internal tags. The public adopter path — published chart
# from charts.k8squad.io with NO image overrides, so every component resolves
# to ghcr.io/k8squad/ksquad-<component>:<chart appVersion> — was exercised by
# no CI at all. That skew let v0.2.0 publish with a pre-install migrate hook
# that passes --migrate-only to an apiserver image predating the flag: a
# brand-new public install failed at the hook (ISI-7277, found in ISI-7264
# WS-1).
#
# What it does, against whatever kube-context is selected (CI points it at a
# throwaway kind cluster):
#
#   1. helm repo add charts.k8squad.io — the PUBLISHED repo, not a local chart
#      dir, so the tested surface is exactly what an adopter pulls.
#   2. Boot a disposable in-cluster Postgres (dev DSN path).
#   3. helm install k8squad-crds, then k8squad with controlPlane.enabled=true
#      and NO image tag settings — defaults MUST resolve to the chart
#      appVersion on ghcr.io. A pre-install hook failure (ksquad-migrate)
#      fails the release; helm --wait covers it.
#   4. Assert every control-plane Deployment resolved to :<appVersion> images
#      (chart/image publish skew guard) and every pod is Ready.
#
# Agent reconcile-to-Ready is credential-gated (BYO LLM creds) and out of scope
# for this lane — a documented skip-with-reason (STEP 5), matching the repo's
# convention in e2e.yml / getting-started-smoke.sh. Never a silent pass.
set -euo pipefail

# ------------------------------------------------------------------ config (overridable)
CHART_REPO_NAME="${CHART_REPO_NAME:-k8squad}"
CHART_REPO_URL="${CHART_REPO_URL:-https://charts.k8squad.io}"
RELEASE="${RELEASE:-k8squad}"
CRDS_RELEASE="${CRDS_RELEASE:-k8squad-crds}"
CP_NS="${CP_NS:-k8squad-system}"                 # control-plane namespace (chart default)
CRDS_NS="${CRDS_NS:-k8squad-crds}"               # CRD chart release namespace (kept separate)
PG_NAME="${PG_NAME:-k8squad-pg-smoke}"           # disposable Postgres for the dev DSN path
PG_NS="${PG_NS:-k8squad-smoke-db}"               # separate ns: the CP ns is chart-owned
PG_IMAGE="${PG_IMAGE:-postgres:16-alpine}"
DEFAULT_MODEL="${DEFAULT_MODEL:-claude-sonnet-4}"
STORAGE_CLASS="${STORAGE_CLASS:-standard}"       # kind default StorageClass
INSTALL_TIMEOUT="${INSTALL_TIMEOUT:-10m}"

fail() { echo "::error::$*" >&2; exit 1; }
note() { echo "::notice::$*" >&2; }
group() { echo "::group::$*"; }
endgroup() { echo "::endgroup::"; }

# ------------------------------------------------------------------ preflight
command -v kubectl >/dev/null || fail "kubectl not on PATH"
command -v helm    >/dev/null || fail "helm not on PATH"
command -v jq      >/dev/null || fail "jq not on PATH"
kubectl cluster-info >/dev/null 2>&1 || fail "no reachable cluster on the current kube-context"

# ============================================================ STEP 1 — published chart repo
group "STEP 1 — helm repo add $CHART_REPO_NAME $CHART_REPO_URL"
helm repo add "$CHART_REPO_NAME" "$CHART_REPO_URL"
helm repo update >/dev/null
CP_CHART_VERSION="$(helm search repo "$CHART_REPO_NAME/k8squad" --output=json | jq -r '.[0].version')"
CP_APPVERSION="$(helm search repo "$CHART_REPO_NAME/k8squad" --output=json | jq -r '.[0].app_version')"
CRDS_CHART_VERSION="$(helm search repo "$CHART_REPO_NAME/k8squad-crds" --output=json | jq -r '.[0].version')"
[ "$CP_CHART_VERSION" != "null" ] && [ -n "$CP_CHART_VERSION" ] || fail "no published k8squad chart found in $CHART_REPO_URL"
[ "$CP_APPVERSION" != "null" ] && [ -n "$CP_APPVERSION" ] || fail "published chart has no appVersion"
echo "published: k8squad chart=$CP_CHART_VERSION appVersion=$CP_APPVERSION crds=$CRDS_CHART_VERSION"
endgroup

# ============================================================ STEP 2 — disposable Postgres
group "STEP 2 — boot disposable Postgres ($PG_IMAGE in $PG_NS)"
# Dev DSN path: the chart renders controlPlane.database.dsn into a chart-owned
# Secret. A public smoke carries no real credentials — a throwaway PG is the
# smallest honest backing store for a fresh-install assertion.
kubectl create namespace "$PG_NS" --dry-run=client -o yaml | kubectl apply -f -
kubectl delete pod "$PG_NAME" -n "$PG_NS" --ignore-not-found >/dev/null
kubectl run "$PG_NAME" --namespace "$PG_NS" --image="$PG_IMAGE" \
  --env POSTGRES_USER=k8squad --env POSTGRES_PASSWORD=k8squad --env POSTGRES_DB=k8squad \
  --restart=Never
kubectl delete service "$PG_NAME" -n "$PG_NS" --ignore-not-found >/dev/null
kubectl expose pod "$PG_NAME" -n "$PG_NS" --port 5432 --name "$PG_NAME"
kubectl wait --for=condition=Ready "pod/$PG_NAME" -n "$PG_NS" --timeout=180s \
  || fail "disposable Postgres never became Ready — lane infra failure, not a product signal"
DSN="postgres://k8squad:k8squad@${PG_NAME}.${PG_NS}.svc.cluster.local:5432/k8squad?sslmode=disable"
endgroup

# ============================================================ STEP 3 — install from the PUBLISHED repo
group "STEP 3 — helm install $CRDS_RELEASE then $RELEASE (published chart, DEFAULT images)"
# ADR-0002 Option B: CRDs chart first. Then the control-plane chart with
# controlPlane.enabled=true and NO image settings — the defaults under test.
# helm --wait blocks on the pre-install/pre-upgrade ksquad-migrate hook Job:
# if --migrate-only is rejected by the resolved apiserver image (the ISI-7277
# failure mode), this install fails closed right here.
helm upgrade --install "$CRDS_RELEASE" "$CHART_REPO_NAME/k8squad-crds" \
  --namespace "$CRDS_NS" --create-namespace \
  --wait --timeout 5m \
  || fail "CRDs chart install failed (published chart=$CRDS_CHART_VERSION)"
helm upgrade --install "$RELEASE" "$CHART_REPO_NAME/k8squad" \
  --namespace "$CP_NS" --create-namespace \
  --set controlPlane.enabled=true \
  --set "controlPlane.database.dsn=$DSN" \
  --set "controlPlane.nats.persistence.storageClassName=$STORAGE_CLASS" \
  --set "modelConfig.default.model=$DEFAULT_MODEL" \
  --wait --timeout "$INSTALL_TIMEOUT" \
  || fail "control-plane install failed with PUBLIC default images (chart=$CP_CHART_VERSION appVersion=$CP_APPVERSION) — this is the ISI-7277 class of failure: the published chart must install on public defaults"
note "install completed with public default images (incl. ksquad-migrate hook Job)"
endgroup

# ============================================================ STEP 4 — image-tag skew guard + pods Ready
group "STEP 4 — every control-plane Deployment resolved to :$CP_APPVERSION and is Ready"
# The chart's default image tag falls back to .Chart.AppVersion (_helpers.tpl).
# If a publish lane ever ships a chart whose appVersion has no matching public
# image (or vice versa), this catches it explicitly instead of as a pull
# error on some unrelated re-run.
SKEW=0
while read -r IMG; do
  case "$IMG" in
    *":$CP_APPVERSION") echo "  ok   $IMG" ;;
    *) echo "  SKEW $IMG (expected tag $CP_APPVERSION)"; SKEW=$((SKEW+1)) ;;
  esac
done < <(kubectl get deployments -n "$CP_NS" -o json \
  | jq -r '.items[].spec.template.spec.containers[].image')
[ "$SKEW" -eq 0 ] || fail "$SKEW control-plane container image(s) did not resolve to the chart appVersion tag $CP_APPVERSION — chart/image publish skew"
kubectl get deployments -n "$CP_NS" -o name | while read -r D; do
  kubectl -n "$CP_NS" rollout status --timeout=300s "$D" \
    || fail "rollout incomplete: $D"
done
echo "control-plane deployments Ready in $CP_NS:"
kubectl get pods -n "$CP_NS" -o wide
endgroup

# ============================================================ STEP 5 — documented skip
group "STEP 5 — agent reconcile-to-Ready: SKIPPED (credential-gated)"
# Reconcile-to-Ready of example agents requires BYO LLM credentials, which a
# public smoke lane must not hold. Scope of this lane is: the PUBLISHED chart
# installs clean with PUBLIC default images (hook + control plane Ready).
# Anything deeper belongs to the credentialed internal lanes.
echo "SKIP(with reason): agent reconcile-to-Ready — BYO LLM credentials required; out of scope for the public-images lane (same convention as e2e.yml STEP 5)."
endgroup

echo "public-images-smoke: PASS (chart=$CP_CHART_VERSION appVersion=$CP_APPVERSION)"
