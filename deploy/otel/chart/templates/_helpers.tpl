{{/*
Helpers for the k8squad-otel chart (ISI-4163).
*/}}

{{- define "k8squad-otel.validate" -}}
{{- $mode := .Values.collector.mode -}}
{{- if not (has $mode (list "operator" "deployment" "external")) -}}
{{- fail (printf "collector.mode must be one of operator|deployment|external, got %q" $mode) -}}
{{- end -}}
{{- if and (eq $mode "external") .Values.nodeLogs.enabled -}}
{{- if not .Values.collector.external.endpoint -}}
{{- fail "collector.external.endpoint is required when collector.mode=external and nodeLogs.enabled=true" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "k8squad-otel.labels" -}}
app.kubernetes.io/part-of: k8squad
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{/* Contract Service name: "<collector.name>-collector". Matches the
     otel-operator's derivation in operator mode and is rendered literally in
     deployment mode, so config/helm's hardcoded
     otel-gateway-collector.observability keeps resolving in both. */}}
{{- define "k8squad-otel.gatewayServiceName" -}}
{{- printf "%s-collector" .Values.collector.name -}}
{{- end -}}

{{/* HTTP endpoint the node-logs DaemonSet exports to. */}}
{{- define "k8squad-otel.gatewayHttpEndpoint" -}}
{{- if eq .Values.collector.mode "external" -}}
{{- .Values.collector.external.endpoint -}}
{{- else -}}
{{- printf "http://%s.%s:4318" (include "k8squad-otel.gatewayServiceName" .) .Release.Namespace -}}
{{- end -}}
{{- end -}}
