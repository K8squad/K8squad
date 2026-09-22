{{/*
Gateway collector config — single source shared by the OpenTelemetryCollector
CR (operator mode) and the ConfigMap (deployment mode). Content is the live
in-cluster pipeline captured in ISI-4153; only endpoints, resource attributes,
and the auth secret reference are parameterized.
Pipeline order is a hard rule: memory_limiter → k8sattributes → resource →
cumulativetodelta/redaction → batch. No tail_sampling: it stays out of the
collector entirely while end-to-end run tracing is being validated at 100%
span capture (ISI-4780, per board directive — extends the ISI-4238 detach).
The processor definition was removed (not just unwired) so sampling cannot be
silently re-enabled during the test window; restore it from git history
(commit e3886a3 / deploy/otel/reference) once tracing is confirmed.
*/}}
{{- define "k8squad-otel.gatewayConfig" -}}
extensions:
  health_check:
    endpoint: 0.0.0.0:13133
    # Self-heal (ISI-4040 / ISI-4371). Report UNHEALTHY on :13133 once the
    # export pipeline stalls, so the liveness probe restarts the pod and clears
    # a wedged exporter instead of silently dropping data for hours (the 11h
    # CP-brownout failure mode). A restart is the proven remedy — it demonstrably
    # cleared the wedged Dynatrace exporter during ISI-4371 recovery.
    check_collector_pipeline:
      enabled: true
      interval: 5m
      exporter_failure_threshold: 5
  # Disk-backed sending queue (ISI-4371 hardening). Buffers to the otelcol-state
  # volume so a transient CP/DNS brownout drains on recovery rather than
  # drop-and-wedge. Survives container restarts (incl. the self-heal restart
  # above) within the pod; not cross-reschedule (emptyDir) — acceptable boundary.
  file_storage/queue:
    directory: /var/lib/otelcol/sending-queue
    create_directory: true
    timeout: 10s
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
  # tail_sampling REMOVED (ISI-4780). No trace sampling runs in the collector
  # while end-to-end run tracing is validated at 100% span capture. The prior
  # ISI-4238 change only detached it from the pipeline but kept the definition
  # (one-line re-enable); the board asked for it fully out so it cannot be
  # silently switched back on mid-test. Restore the processor + wire it before
  # `batch` from git history (commit e3886a3) once tracing is confirmed.
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
    # Buffer across vendor/DNS outages instead of dropping (ISI-4371). The
    # disk-backed queue drains when the endpoint recovers; retry backs off but
    # never gives up (max_elapsed_time: 0) so a multi-hour CP brownout does not
    # permanently drop telemetry — it accrues export failures, which trips the
    # health_check self-heal above and makes the outage loud, not silent.
    sending_queue:
      enabled: true
      storage: file_storage/queue
      num_consumers: 10
      queue_size: 10000
    retry_on_failure:
      enabled: true
      initial_interval: 5s
      max_interval: 30s
      max_elapsed_time: 0
  debug:
    verbosity: basic
service:
  extensions: [health_check, file_storage/queue]
  pipelines:
    traces:
      receivers: [otlp]
      # No tail_sampling (ISI-4780). Trace sampling is kept out of the collector
      # while engine run/LLM spans are validated end-to-end — the old base-rate
      # probabilistic policy dropped ~90% of successful-run traces and made that
      # validation unreliable. Re-add `tail_sampling` (processor + this list)
      # once tracing is confirmed flowing at 100% capture.
      processors: [memory_limiter, k8sattributes, resource, redaction, transform/redaction, batch]
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
    # Both kubelet layouts: containerd/CRI writes 3-segment paths
    # (<ns>_<pod>_<uid>/<restart>/<container>.log); some runtimes/enhancers
    # write 4-segment (<ns>/<pod>/<uid>/<container>.log). Reading both costs
    # nothing — non-matching globs are skipped.
    include:
      - /var/log/pods/*/*/*.log
      - /var/log/pods/*/*/*/*.log
    # Exclude the telemetry pipeline's OWN pod logs to break the self-ingestion
    # loop (ISI-4372). node-logs tails /var/log/pods/* — which includes the
    # gateway collector's and node-logs' own logs in the release namespace.
    # Under load, collector logging (export retries, batch/queue chatter) about
    # ingesting logs generates more logs, a positive-feedback amplifier that
    # drove ~54k logs/s (~208M rows/60m), 100% service.name=k8s-node-logs in the
    # observability namespace — pure cost and export-path pressure (see the
    # ISI-4371 wedge). Scoped to the collectors by pod-name prefix (NOT the whole
    # namespace) so real app logs co-located in that namespace still flow.
    # Both kubelet layouts are covered: CRI 3-segment (<ns>_<pod>_<uid>/...) and
    # 4-segment (<ns>/<pod>/<uid>/...); ** spans the <uid|restart>/<container>
    # tail (doublestar, supported by the filelog matcher).
    exclude:
      - "*.gz"
      # gateway collector (operator mode: <name>-collector-*; deployment mode: <name>-*)
      - /var/log/pods/{{ .Release.Namespace }}_{{ .Values.collector.name }}*/**/*.log
      - /var/log/pods/{{ .Release.Namespace }}/{{ .Values.collector.name }}*/**/*.log
      # node-logs DaemonSet (this collector itself — the feedback-loop source)
      - /var/log/pods/{{ .Release.Namespace }}_otel-node-logs*/**/*.log
      - /var/log/pods/{{ .Release.Namespace }}/otel-node-logs*/**/*.log
    start_at: end
    include_file_path: true
    operators:
      # Primary layout — standard kubelet/CRI path used by containerd AND
      # docker-shim-via-CRI (verified empirically, ISI-4137 hardening):
      #   /var/log/pods/<namespace>_<pod>_<uid>/<restart>/<container>.log
      # Pod/namespace names cannot contain "_" (RFC 1123), so [^_]+ splits are
      # unambiguous; the uid is the 36-char k8s UUID.
      - type: regex_parser
        id: parse_cri_path
        parse_from: attributes["log.file.path"]
        # Only attempt when the path carries the CRI "_<uuid>/<restart>/"
        # signature — avoids a per-entry mismatch error on 4-segment layouts.
        if: 'attributes["log.file.path"] matches "_[a-f0-9-]{36}/[0-9]+/"'
        regex: '/var/log/pods/(?P<namespace>[^_/]+)_(?P<pod>[^_/]+)_(?P<uid>[a-f0-9\-]{36})/\d+/(?P<container>[^/]+)\.log$'
      # Fallback layout — as captured live in ISI-4153 (enhanced-runtime dirs):
      #   /var/log/pods/<namespace>/<pod>/<uid>/<container>.log
      # Guarded so it only runs when the CRI parser above did not match.
      - type: regex_parser
        id: parse_alt_path
        parse_from: attributes["log.file.path"]
        if: 'attributes.pod == nil'
        regex: '/var/log/pods/(?P<namespace>[^/]+)/(?P<pod>[^/]+)/(?P<uid>[^/]+)/(?P<container>[^\._]+)[^/]*\.log$'
      # Guarded moves: neither layout is guaranteed, and a move on a missing
      # field errors per-entry — only move what a parser actually produced.
      - type: move
        from: attributes.namespace
        to: resource["k8s.namespace.name"]
        if: 'attributes.namespace != nil'
      - type: move
        from: attributes.pod
        to: resource["k8s.pod.name"]
        if: 'attributes.pod != nil'
      - type: move
        from: attributes.container
        to: resource["k8s.container.name"]
        if: 'attributes.container != nil'
      - type: move
        from: attributes.uid
        to: resource["k8s.pod.uid"]
        if: 'attributes.uid != nil'
      # CRI line format (containerd): "<ts> stdout|stderr F|P <message>".
      # Not JSON — parsing it with json_parser fails per line (verified).
      - type: regex_parser
        id: parse_cri_line
        parse_from: body
        if: 'body != nil and not (body matches "^[{]")'
        regex: '^(?P<time>\d{4}-\d{2}-\d{2}T[^ ]+) (?P<stream>stdout|stderr) (?P<flag>[FP]) ?(?P<log>.*)$'
        timestamp:
          parse_from: attributes.time
          layout_type: gotime
          layout: '2006-01-02T15:04:05.999999999Z07:00'
      # docker-json line format (docker nodes: kubelet symlinks into
      # /var/lib/docker/containers): {"log": ..., "time": ...}
      - type: json_parser
        id: parse_runtime_line
        parse_from: body
        if: 'body != nil and body matches "^[{]"'
        timestamp:
          parse_from: attributes.time
          layout_type: gotime
          layout: '2006-01-02T15:04:05.999999999Z07:00'
      - type: move
        from: attributes.log
        to: body
        if: 'attributes.log != nil'
      - type: move
        from: attributes.stream
        to: attributes["log.iostream"]
        if: 'attributes.stream != nil'
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
