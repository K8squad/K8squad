#!/usr/bin/env bash
# ISI-3518 / ADR-0002 (Option B): wrap a controller-gen CRD (plain YAML from
# config/crd/bases) into an upgrade-safe Helm *template* for the standalone
# k8squad-crds chart, and print it to stdout.
#
# Two metadata mutations, nothing else — the schema body is passed through
# verbatim so numeric openAPIV3Schema fields (minimum/maximum/default) are never
# reformatted:
#   1. `helm.sh/resource-policy: keep` is injected into the metadata.annotations
#      block, gated by `{{- if .Values.keep }}`, so `helm uninstall k8squad-crds`
#      never deletes the CRD (and therefore never cascades a delete of the
#      user's custom resources).
#   2. `app.kubernetes.io/part-of: k8squad` label so the CRDs are selectable as
#      a set (`kubectl get crd -l app.kubernetes.io/part-of=k8squad`) — the
#      selector the chart NOTES and the CI propagation test rely on.
#
# The CRDs live as ordinary templates in this standalone chart (NOT Helm's
# install-only crds/ dir), which is what makes `helm upgrade k8squad-crds`
# reconcile CRD schema changes into existing installs — independently of the
# control-plane chart (ADR-0002 §3/§6). Installing this chart is the opt-in;
# there is no crds.install toggle (ADR §2).
#
# The same helper is used by `make helm-sync-crds` (production sync) and by
# hack/test-crd-upgrade.sh (old-chart simulation) so both wrap identically.
set -euo pipefail

src="${1:?usage: wrap-crd-template.sh <config/crd/bases/ksquad.io_*.yaml>}"

awk '
  /^  annotations:$/ && !done {
    print "  labels:"
    print "    app.kubernetes.io/part-of: k8squad"
    print
    print "    {{- if .Values.keep }}"
    print "    helm.sh/resource-policy: keep"
    print "    {{- end }}"
    done = 1
    next
  }
  { print }
' "$src"
