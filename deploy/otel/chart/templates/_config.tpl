{{/*
Gateway collector config — single source shared by the OpenTelemetryCollector
CR (operator mode) and the ConfigMap (deployment mode). Content is the live
in-cluster pipeline captured in ISI-4153; only endpoints, resource attributes,
and the auth secret reference are parameterized.
Pipeline order is a hard rule: memory_limiter → k8sattributes → resource →
cumulativetodelta/redaction → tail_sampling → batch.
*/}}
{{- define "k8squad-otel.gatewayConfig" -}}
extensions:
  health_check:
    endpoint: 0.0.0.0:13133
receivers:
  otlp:
    protocols:
      grpc:
        endpoint: 0.0.0.0:4317
      http:
        endpoint: 0.0.0.0:4318
processors:
  memory_limiter:
    check_interval: 1s
    limit_percentage: 80
    spike_limit_percentage: 25
  k8sattributes:
    auth_type: serviceAccount
    passthrough: false
    extract:
      metadata:
        - k8s.namespace.name
        - k8s.pod.name
        - k8s.pod.uid
        - k8s.deployment.name
        - k8s.node.name
    pod_association:
      - sources:
          - from: resource_attribute
            name: k8s.pod.ip
      - sources:
          - from: connection
  resource:
    attributes:
      - key: service.namespace
        value: {{ .Values.collector.serviceNamespace | quote }}
        action: upsert
      - key: deployment.environment
        value: {{ .Values.collector.environment | quote }}
        action: upsert
  cumulativetodelta: {}
  redaction:
    allow_all_keys: true
    blocked_values:
      - "(?i)bearer\\s+[a-z0-9._~+/-]+=*"
      - "sk-[A-Za-z0-9]{16,}"
      - "xox[baprs]-[A-Za-z0-9-]+"
      - "eyJ[A-Za-z0-9_-]+\\.[A-Za-z0-9_-]+\\.[A-Za-z0-9_-]+"
      - "[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\\.[a-zA-Z]{2,}"
    summary: debug
  transform/redaction:
    error_mode: ignore
    trace_statements:
      - context: span
        statements:
          - delete_matching_keys(attributes, "(?i).*(token|secret|password|authorization|api[-_]?key|credential).*")
    log_statements:
      - context: log
        statements:
          - delete_matching_keys(attributes, "(?i).*(token|secret|password|authorization|api[-_]?key|credential).*")
          - replace_pattern(body, "(?i)bearer\\s+[a-z0-9._~+/-]+=*", "Bearer [REDACTED]")
          - replace_pattern(body, "sk-[A-Za-z0-9]{16,}", "[REDACTED_KEY]")
          - replace_pattern(body, "xox[baprs]-[A-Za-z0-9-]+", "[REDACTED_TOKEN]")
          - replace_pattern(body, "eyJ[A-Za-z0-9_-]+\\.[A-Za-z0-9_-]+\\.[A-Za-z0-9_-]+", "[REDACTED_JWT]")
          - replace_pattern(body, "[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\\.[a-zA-Z]{2,}", "[REDACTED_EMAIL]")
  tail_sampling:
    decision_wait: 10s
    num_traces: 50000
    expected_new_traces_per_sec: 200
    policies:
      - name: keep-errors
        type: status_code
        status_code:
          status_codes: [ERROR]
      - name: keep-failed-runs
        type: string_attribute
        string_attribute:
          key: ksquad.run.terminal_reason
          values: [failed, error, killed]
      - name: keep-paused-runs
        type: string_attribute
        string_attribute:
          key: ksquad.run.phase
          values: [Paused]
      - name: base-rate
        type: probabilistic
        probabilistic:
          sampling_percentage: 10
  batch:
    send_batch_size: 8192
    send_batch_max_size: 16384
    timeout: 5s
exporters:
  otlphttp/vendor:
    endpoint: {{ .Values.collector.vendor.endpoint | quote }}
    headers:
      Authorization: "${env:KSQUAD_OTLP_AUTH}"
    tls:
      insecure: false
  debug:
    verbosity: basic
service:
  extensions: [health_check]
  pipelines:
    traces:
      receivers: [otlp]
      processors: [memory_limiter, k8sattributes, resource, redaction, transform/redaction, tail_sampling, batch]
      exporters: [otlphttp/vendor]
    metrics:
      receivers: [otlp]
      processors: [memory_limiter, k8sattributes, resource, cumulativetodelta, batch]
      exporters: [otlphttp/vendor]
    logs:
      receivers: [otlp]
      processors: [memory_limiter, k8sattributes, resource, redaction, transform/redaction, batch]
      exporters: [otlphttp/vendor]
{{- end -}}

{{/*
Node-logs collector config (filelog → gateway). The gateway endpoint resolves
by mode: in-chart Service, or collector.external.endpoint in external mode.
*/}}
{{- define "k8squad-otel.nodeLogsConfig" -}}
extensions:
  health_check:
    endpoint: 0.0.0.0:13133
receivers:
  filelog/pods:
    include: [ /var/log/pods/*/*/*.log ]
    exclude: [ "*.gz" ]
    start_at: end
    include_file_path: true
    operators:
      - type: regex_parser
        id: parse_pod_path
        regex: '/var/log/pods/(?P<namespace>[^/]+)/(?P<pod>[^/]+)/(?P<uid>[^/]+)/(?P<container>[^\._]+).*\.log$'
        parse_from: attributes["log.file.path"]
      - type: move
        from: attributes.namespace
        to: resource["k8s.namespace.name"]
      - type: move
        from: attributes.pod
        to: resource["k8s.pod.name"]
      - type: move
        from: attributes.container
        to: resource["k8s.container.name"]
      - type: move
        from: attributes.uid
        to: resource["k8s.pod.uid"]
      - type: json_parser
        id: parse_runtime_line
        parse_from: body
        timestamp:
          parse_from: attributes.time
          layout_type: gotime
          layout: '2006-01-02T15:04:05.000000000Z07:00'
processors:
  memory_limiter:
    check_interval: 1s
    limit_percentage: 80
    spike_limit_percentage: 25
  k8sattributes:
    auth_type: serviceAccount
    passthrough: false
    extract:
      metadata:
        - k8s.namespace.name
        - k8s.pod.name
        - k8s.pod.uid
        - k8s.deployment.name
        - k8s.node.name
    pod_association:
      - sources:
          - from: resource_attribute
            name: k8s.pod.uid
      - sources:
          - from: resource_attribute
            name: k8s.pod.ip
      - sources:
          - from: connection
  resource:
    attributes:
      - key: service.name
        value: k8s-node-logs
        action: upsert
      - key: service.namespace
        value: {{ .Values.collector.serviceNamespace | quote }}
        action: upsert
      - key: deployment.environment
        value: {{ .Values.collector.environment | quote }}
        action: upsert
  redaction:
    allow_all_keys: true
    blocked_values:
      - "(?i)bearer\\s+[a-z0-9._~+/-]+=*"
      - "sk-[A-Za-z0-9]{16,}"
      - "xox[baprs]-[A-Za-z0-9-]+"
      - "eyJ[A-Za-z0-9_-]+\\.[A-Za-z0-9_-]+\\.[A-Za-z0-9_-]+"
      - "[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\\.[a-zA-Z]{2,}"
    summary: debug
  transform/redaction:
    error_mode: ignore
    log_statements:
      - context: log
        statements:
          - delete_matching_keys(attributes, "(?i).*(token|secret|password|authorization|api[-_]?key|credential).*")
          - replace_pattern(body, "(?i)bearer\\s+[a-z0-9._~+/-]+=*", "Bearer [REDACTED]")
          - replace_pattern(body, "sk-[A-Za-z0-9]{16,}", "[REDACTED_KEY]")
          - replace_pattern(body, "xox[baprs]-[A-Za-z0-9-]+", "[REDACTED_TOKEN]")
          - replace_pattern(body, "eyJ[A-Za-z0-9_-]+\\.[A-Za-z0-9_-]+\\.[A-Za-z0-9_-]+", "[REDACTED_JWT]")
          - replace_pattern(body, "[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\\.[a-zA-Z]{2,}", "[REDACTED_EMAIL]")
  batch:
    send_batch_size: 8192
    send_batch_max_size: 16384
    timeout: 5s
exporters:
  otlphttp/gateway:
    endpoint: {{ include "k8squad-otel.gatewayHttpEndpoint" . | quote }}
service:
  extensions: [health_check]
  pipelines:
    logs:
      receivers: [filelog/pods]
      processors: [memory_limiter, k8sattributes, resource, redaction, transform/redaction, batch]
      exporters: [otlphttp/gateway]
{{- end -}}
