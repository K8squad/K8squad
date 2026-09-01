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

{{/*
Control-plane namespace resolver — every workload lands in the chart namespace.
*/}}
{{- define "k8squad.namespace" -}}
{{- .Values.namespace.name | default "k8squad-system" -}}
{{- end -}}

{{/*
Per-component selector labels. `ctx` is a dict {root, component}. The
`ksquad.io/component` selector is stable across chart versions so Services and
later epics can bind to a workload without pinning app version.
*/}}
{{- define "k8squad.componentSelectorLabels" -}}
app.kubernetes.io/name: {{ include "k8squad.name" .root }}
app.kubernetes.io/instance: {{ .root.Release.Name }}
ksquad.io/component: {{ .component }}
{{- end -}}

{{/*
Per-component full labels (common labels + component selector).
*/}}
{{- define "k8squad.componentLabels" -}}
{{ include "k8squad.labels" .root }}
ksquad.io/component: {{ .component }}
{{- end -}}

{{/*
Container image ref for a component. `ctx` is a dict {root, component}.
Registry+repo default to ghcr.io/k8squad/ksquad-<component>; tag falls back to
the chart appVersion so a bare `controlPlane.enabled=true` pins a real image.
*/}}
{{- define "k8squad.image" -}}
{{- $img := .root.Values.controlPlane.image -}}
{{- $registry := $img.registry | default "ghcr.io/k8squad" -}}
{{- $tag := $img.tag | default .root.Chart.AppVersion -}}
{{- printf "%s/ksquad-%s:%s" $registry .component $tag -}}
{{- end -}}

{{/*
NATS JetStream replicas — single-replica default; nats.ha.enabled flips to a
clustered RAFT quorum.
*/}}
{{- define "k8squad.nats.replicas" -}}
{{- if .Values.controlPlane.nats.ha.enabled -}}{{ .Values.controlPlane.nats.ha.replicas }}{{- else -}}1{{- end -}}
{{- end -}}

{{/*
StorageClass for the JetStream file-store PVC. Its own knob (§16.2) — no silent
cluster-default fallback; nats.yaml fails fast when this is empty.
*/}}
{{- define "k8squad.storageClass.nats" -}}
{{- .Values.controlPlane.nats.persistence.storageClassName -}}
{{- end -}}

{{/*
NATS client URL for the relay. Precedence: an explicit eventRelay.natsUrl
(external bus) wins; else, when the bundled bus is on, derive it from the
chart-owned Service; else empty (event-relay fails closed on empty).
*/}}
{{- define "k8squad.nats.url" -}}
{{- if .Values.controlPlane.eventRelay.natsUrl -}}
{{- .Values.controlPlane.eventRelay.natsUrl -}}
{{- else if .Values.controlPlane.nats.enabled -}}
{{- printf "nats://ksquad-nats.%s.svc:%v" (include "k8squad.namespace" .) (.Values.controlPlane.nats.port | default 4222) -}}
{{- end -}}
{{- end -}}

{{/*
Effective event-relay enablement — explicitly on, OR implied by the bundled bus
(controlPlane.nats.enabled auto-wires the relay so `controlPlane.enabled` alone
yields a complete event plane).
*/}}
{{- define "k8squad.eventRelay.enabled" -}}
{{- if or .Values.controlPlane.eventRelay.enabled .Values.controlPlane.nats.enabled -}}true{{- end -}}
{{- end -}}

{{/*
OTLP exporter env block, shared by every control-plane workload. Emits the
standard OTEL_EXPORTER_OTLP_* vars pointing at the observability gateway
collector (live since ISI-3484). Binaries currently log via pkg/telemetry
(stdout); this is the forward-wiring for the ISI-3103 OTLP spine and is inert
until a binary opts an OTLP exporter in.
*/}}
{{- define "k8squad.otelEnv" -}}
{{- if .Values.controlPlane.otel.enabled }}
- name: OTEL_EXPORTER_OTLP_ENDPOINT
  value: {{ .Values.controlPlane.otel.endpoint | quote }}
- name: OTEL_EXPORTER_OTLP_PROTOCOL
  value: {{ .Values.controlPlane.otel.protocol | quote }}
{{- end }}
{{- end -}}

{{/*
DATABASE_URL env sourced from the configured secret (or the chart-rendered
dev secret). Callers that need Postgres include this block.
*/}}
{{- define "k8squad.databaseEnv" -}}
- name: DATABASE_URL
  valueFrom:
    secretKeyRef:
      {{- if .Values.controlPlane.database.existingSecret }}
      name: {{ .Values.controlPlane.database.existingSecret }}
      key: {{ .Values.controlPlane.database.key | default "dsn" }}
      {{- else }}
      name: {{ include "k8squad.fullname" . }}-database
      key: dsn
      {{- end }}
{{- end -}}

{{/*
Bootstrap-admin env (feature 15.2, ISI-3530). Emitted only when
controlPlane.apiserver.bootstrapAdmin.enabled=true. The password ALWAYS comes
from a Secret — never plaintext values — so we fail-fast if the operator turned
the seed on without supplying a username and existingSecret.
*/}}
{{- define "k8squad.bootstrapAdminEnv" -}}
{{- $ba := .Values.controlPlane.apiserver.bootstrapAdmin | default dict -}}
{{- if $ba.enabled -}}
{{- if not $ba.username }}{{ fail "controlPlane.apiserver.bootstrapAdmin.enabled=true requires controlPlane.apiserver.bootstrapAdmin.username" }}{{- end }}
{{- if not $ba.existingSecret }}{{ fail "controlPlane.apiserver.bootstrapAdmin.enabled=true requires controlPlane.apiserver.bootstrapAdmin.existingSecret (the password is never read from plaintext values)" }}{{- end }}
- name: KSQUAD_BOOTSTRAP_ADMIN_USERNAME
  value: {{ $ba.username | quote }}
- name: KSQUAD_BOOTSTRAP_ADMIN_PASSWORD
  valueFrom:
    secretKeyRef:
      name: {{ $ba.existingSecret }}
      key: {{ $ba.passwordKey | default "password" }}
{{- with $ba.teamId }}
- name: KSQUAD_BOOTSTRAP_ADMIN_TEAM_ID
  value: {{ . | quote }}
{{- end }}
{{- end -}}
{{- end -}}
