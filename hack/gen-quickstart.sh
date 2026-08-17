#!/usr/bin/env bash
# Assemble the published single-file quickstart manifest (ISI-2632).
#
# Source of truth is hack/quickstart/squad.yaml; this script stamps a generated
# header (chart appVersion + provenance) onto it and writes dist/quickstart.yaml,
# the artifact published to https://charts.k8squad.io/quickstart.yaml.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SRC="${ROOT}/hack/quickstart/squad.yaml"
OUT="${OUT:-${ROOT}/dist/quickstart.yaml}"
CHART_DIR="${CHART_DIR:-${ROOT}/config/helm}"

# appVersion is best-effort provenance; never fail the build if helm/grep miss.
APPVER="$(grep -E '^appVersion:' "${CHART_DIR}/Chart.yaml" | awk '{gsub(/"/,"",$2); print $2}' || true)"
APPVER="${APPVER:-unknown}"

mkdir -p "$(dirname "$OUT")"
{
  echo "# K8squad quickstart squad — apply after installing the operator chart."
  echo "#   kubectl apply -f https://charts.k8squad.io/quickstart.yaml"
  echo "# Generated from hack/quickstart/squad.yaml — do not edit in place."
  echo "# appVersion: ${APPVER}"
  cat "$SRC"
} > "$OUT"

echo "wrote ${OUT} (appVersion=${APPVER})"
