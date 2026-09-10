{{/*
Chart name / fullname / labels — standard Helm helpers.
*/}}
{{- define "ksquad.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "ksquad.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "ksquad.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
app.kubernetes.io/name: {{ include "ksquad.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: ksquad
{{- end -}}

{{/*
Component service names.
*/}}
{{- define "ksquad.console.fullname" -}}
{{- printf "%s-console" (include "ksquad.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "ksquad.apiserver.fullname" -}}
{{- printf "%s-apiserver" (include "ksquad.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "ksquad.operator.fullname" -}}
{{- printf "%s-operator" (include "ksquad.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "ksquad.scm-webhook.fullname" -}}
{{- printf "%s-scm-webhook" (include "ksquad.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "ksquad.event-relay.fullname" -}}
{{- printf "%s-event-relay" (include "ksquad.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
NATS/JetStream event-bus names + resolution (ISI-2253, §16/§17.4).
*/}}
{{- define "ksquad.nats.fullname" -}}
{{- printf "%s-nats" (include "ksquad.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Replicas: single-replica default; nats.ha.enabled → clustered JetStream. */}}
{{- define "ksquad.nats.replicas" -}}
{{- if .Values.nats.ha.enabled -}}{{ .Values.nats.ha.replicas }}{{- else -}}1{{- end -}}
{{- end -}}

{{/* NATS client URL for the relay — release-derived unless overridden. */}}
{{- define "ksquad.nats.url" -}}
{{- if .Values.events.relay.natsUrl -}}
{{- .Values.events.relay.natsUrl -}}
{{- else -}}
{{- printf "nats://%s.%s.svc:%v" (include "ksquad.nats.fullname" .) .Release.Namespace .Values.nats.port -}}
{{- end -}}
{{- end -}}

{{/*
StorageClass resolution — per-family override, else the global value.
Emits empty string when neither is set; ksquad.validate turns that into a
fail-fast (we NEVER fall back to the cluster default). ISI-2149 / §16.2.
*/}}
{{- define "ksquad.storageClass.postgres" -}}
{{- .Values.storage.postgres.storageClassName | default .Values.storage.storageClassName -}}
{{- end -}}

{{- define "ksquad.storageClass.workspace" -}}
{{- .Values.storage.workspace.storageClassName | default .Values.storage.storageClassName -}}
{{- end -}}

{{- define "ksquad.storageClass.nats" -}}
{{- .Values.storage.nats.storageClassName | default .Values.storage.storageClassName -}}
{{- end -}}

{{/*
ksquad.validate — fail-fast pre-flight for the two things this chart must
never guess: exposure and storage. Included once from an always-rendered
template so a bad values file fails `helm install`/`template` with a clear
message rather than a dangling route or a cluster-default PVC.
*/}}
{{- define "ksquad.validate" -}}
{{- $mode := .Values.exposure.mode -}}
{{- if not (has $mode (list "gateway" "ingress" "clusterip")) -}}
{{- fail (printf "exposure.mode must be one of gateway|ingress|clusterip, got %q" $mode) -}}
{{- end -}}
{{- if ne $mode "clusterip" -}}
  {{- if not .Values.exposure.hostnames.console -}}
  {{- fail "exposure.hostnames.console is REQUIRED for gateway/ingress modes" -}}
  {{- end -}}
  {{- if not .Values.exposure.hostnames.apiserver -}}
  {{- fail "exposure.hostnames.apiserver is REQUIRED for gateway/ingress modes (the SSE endpoint, §13)" -}}
  {{- end -}}
{{- end -}}
{{- if eq $mode "gateway" -}}
  {{- if not .Values.exposure.gateway.gatewayClassName -}}
  {{- fail "exposure.gateway.gatewayClassName is REQUIRED when exposure.mode=gateway (cilium|envoy|istio|traefik) — the chart references, never creates, a GatewayClass" -}}
  {{- end -}}
  {{- if and .Values.exposure.gateway.listeners.https.enabled (not .Values.exposure.gateway.listeners.https.certSecretName) -}}
  {{- fail "exposure.gateway.listeners.https.certSecretName is REQUIRED when the https listener is enabled (TLS terminates at the gateway)" -}}
  {{- end -}}
  {{- if and (not .Values.exposure.gateway.listeners.http.enabled) (not .Values.exposure.gateway.listeners.https.enabled) -}}
  {{- fail "at least one exposure.gateway.listeners.{http,https}.enabled must be true — a Gateway with zero listeners is invalid and leaves the HTTPRoutes' sectionName dangling (ISI-2286 F2)" -}}
  {{- end -}}
{{- end -}}
{{- if eq $mode "ingress" -}}
  {{- if not .Values.exposure.ingress.className -}}
  {{- fail "exposure.ingress.className is REQUIRED when exposure.mode=ingress" -}}
  {{- end -}}
  {{- if and .Values.exposure.ingress.tls.enabled (not .Values.exposure.ingress.tls.secretName) -}}
  {{- fail "exposure.ingress.tls.secretName is REQUIRED when ingress TLS is enabled" -}}
  {{- end -}}
{{- end -}}
{{- if not (include "ksquad.storageClass.postgres" .) -}}
{{- fail "storage.storageClassName (or storage.postgres.storageClassName) is REQUIRED — KSquad never relies on the cluster-default StorageClass (§16.2)" -}}
{{- end -}}
{{- if not (include "ksquad.storageClass.workspace" .) -}}
{{- fail "storage.storageClassName (or storage.workspace.storageClassName) is REQUIRED — KSquad never relies on the cluster-default StorageClass (§16.2)" -}}
{{- end -}}
{{- if .Values.nats.enabled -}}
  {{- if not (include "ksquad.storageClass.nats" .) -}}
  {{- fail "storage.storageClassName (or storage.nats.storageClassName) is REQUIRED when nats.enabled — the JetStream PVC never relies on the cluster-default StorageClass (§16.2)" -}}
  {{- end -}}
  {{- if .Values.nats.ha.enabled -}}
    {{- $r := int .Values.nats.ha.replicas -}}
    {{- if lt $r 3 -}}
    {{- fail "nats.ha.replicas must be >= 3 for a clustered JetStream RAFT quorum (single-replica is the non-HA default)" -}}
    {{- end -}}
    {{- if eq (mod $r 2) 0 -}}
    {{- fail "nats.ha.replicas must be ODD for a JetStream RAFT quorum (3, 5, …)" -}}
    {{- end -}}
  {{- end -}}
{{- end -}}
{{- end -}}
