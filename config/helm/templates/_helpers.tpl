{{/*
Expand the name of the chart.
*/}}
{{- define "k8squad.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Create a default fully qualified app name (63 char limit, per DNS label rules).
*/}}
{{- define "k8squad.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s" (include "k8squad.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{/*
Common labels applied to chart-owned resources.
*/}}
{{- define "k8squad.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "k8squad.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: k8squad
{{- end -}}

{{/*
Selector labels. The control-plane namespace carries only stable selectors so
that later epics can match resources without binding to chart version.
*/}}
{{- define "k8squad.selectorLabels" -}}
app.kubernetes.io/name: {{ include "k8squad.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}
