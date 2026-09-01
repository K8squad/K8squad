{{/*
Chart name.
*/}}
{{- define "k8squad-crds.name" -}}
{{- .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Common labels. CRDs are cluster-scoped and long-lived (resource-policy: keep),
so we deliberately keep only stable, version-independent labels on them — no
helm.sh/chart or app.kubernetes.io/version, which would churn every release and
is meaningless on an object Helm is told never to delete.
*/}}
{{- define "k8squad-crds.labels" -}}
app.kubernetes.io/name: {{ include "k8squad-crds.name" . }}
app.kubernetes.io/part-of: k8squad
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}
