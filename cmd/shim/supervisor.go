/*
Copyright 2026 The K8squad Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// supervisor.go — the in-pod supervisor mode (ADR-0007 D1/D2, M1.2): when the
// sandbox pod's entrypoint runs `shim supervisor`, this process is the pod's
// PID 1 serving the kube contract the warm-pool Boot spec already probes:
//
//	GET /health      liveness — the supervisor process is serving.
//	GET /ready       readiness — the supervisor is up (warm pods go Ready
//	                WITHOUT a bound Run; readiness is not gated on the
//	                credential so unbound pool stock stays claimable).
//	GET /handshake   the Bind→pod handshake (D2): 200 once the run-scoped
//	                task-io credential files have landed under
//	                taskio.CoordMountPath and been parsed — the operator-side
//	                proof that the Secret the Bind-path writer dropped actually
//	                reached the pod. 503 (with detail) before.
//	POST /task       the D1 task envelope endpoint: accepts one A2A Task as
//	                JSON, drives it through the runtime, streams the JSONL
//	                run-event sequence back over the response body — the same
//	                wire contract `shim run` speaks on stdio, so a future
//	                PodProxyTransport bridges to it unchanged.
//
// The credential wait is a poll loop (1s tick), not a fsnotify: the projected
// Secret volume materializes on the kubelet's sync cadence, and ADR-0007
// explicitly accepts that latency ("the supervisor waits on the file rather
// than racing it"). Once the files land, the supervisor Extracts the W3C
// carrier they carry and initializes the telemetry spine with it, so every
// span the supervisor/runtime emits joins the Run's distributed trace — the
// M1.2 telemetry leg.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/K8squad/K8squad/pkg/a2a"
	"github.com/K8squad/K8squad/pkg/shim"
	"github.com/K8squad/K8squad/pkg/shim/runtimes"
	"github.com/K8squad/K8squad/pkg/taskio"
	"github.com/K8squad/K8squad/pkg/telemetry"
	"github.com/K8squad/K8squad/pkg/telemetry/toolusage"
)

// supervisorAddr is the listen address. The kube Boot spec probes :8080, which
// is also the ADR-0007 contract; the env override exists for tests and
// non-kube dev runs.
const supervisorAddr = ":8080"

// supervisorCoordMountPath is the credential mount the wait loop polls — a var
// so tests can point it at a temp dir (the const it defaults to is the
// contract).
var supervisorCoordMountPath = taskio.CoordMountPath

// runSupervisor is the `shim supervisor` entrypoint. It never returns nil on
// server failure — a supervisor that cannot serve is a dead sandbox, and the
// kubelet should restart the pod (CrashLoopBackOff is honest degradation).
func runSupervisor(args []string) error {
	addr := supervisorAddr
	if v := os.Getenv("KSQUAD_SUPERVISOR_ADDR"); v != "" {
		addr = v
	}
	if len(args) > 0 && args[0] != "" && os.Getenv("KSQUAD_SUPERVISOR_ADDR") == "" {
		addr = args[0]
	}

	ctx := context.Background()
	_, supSpan := telemetry.Tracer().Start(ctx, "supervisor.start",
		trace.WithAttributes(
			attribute.String("ksquad.supervisor.addr", addr),
			attribute.String("ksquad.pod.name", os.Getenv("HOSTNAME")),
		))
	defer supSpan.End()

	sup := &supervisor{
		// toolusage gate mirrors `shim run` (default on; Boot stamps the
		// operator's OTelConfig-derived toggle into the sandbox env).
		toolUsage: env("KSQUAD_TOOL_USAGE_ENABLED", "true") != "false",
		// A real registry backs the tool-usage mapper so the ksquad_tool_* /
		// ksquad_skill_* / ksquad_llm_* series actually register and increment
		// in supervisor-mode pods (ISI-4385: `shim supervisor` used to pass a
		// NIL registry, so the metrics were built but never registered — every
		// increment fell into the void and nothing was ever scrapeable). Unlike
		// `shim run` (a one-shot process that dumps a textfile at exit), the
		// supervisor is long-lived and exposes the exposition on GET /metrics.
		metricsReg:     prometheus.NewRegistry(),
		supervisorSpan: supSpan,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", sup.handleHealth)
	mux.HandleFunc("GET /ready", sup.handleReady)
	mux.HandleFunc("GET /handshake", sup.handleHandshake)
	mux.HandleFunc("POST /task", sup.handleTask)
	// D2 pull surface: the in-pod tool-usage exposition. The mapper registers
	// its ksquad_* set on sup.metricsReg the moment the first /task builds the
	// engine; before that the endpoint serves an empty (but live) exposition.
	mux.Handle("GET /metrics", promhttp.HandlerFor(sup.metricsReg, promhttp.HandlerOpts{}))

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Credential wait loop: starts BEFORE the server so the first /ready or
	// /handshake probe can already observe a landed credential.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go sup.awaitCredential(ctx)

	// Telemetry lifetime = supervisor lifetime: the OTel global providers
	// the handshake initializes must stay up for the pod's whole life, so
	// their shutdown runs here at process exit — never inside
	// awaitCredential, whose defer would fire at handshake completion and
	// kill the trace/metric/log pipelines for the rest of the pod's run
	// (ISI-4161 review).
	defer func() {
		if sd := sup.teardownTelemetry(); sd != nil {
			_ = sd(context.Background())
		}
	}()

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	fmt.Fprintf(os.Stderr, "shim supervisor: serving on %s (waiting for credential under %s)\n", addr, taskio.CoordMountPath)

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("supervisor server: %w", err)
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

// supervisor is the shared state of the in-pod control surface.
type supervisor struct {
	toolUsage bool
	// metricsReg backs the tool-usage mapper (ISI-4385): a real registry the
	// engine's mapper registers its ksquad_* set on, served on GET /metrics.
	// Created once at startup so the mux handler and the lazily-built engine
	// share the same registry.
	metricsReg *prometheus.Registry
	// supervisorSpan is the root span for this supervisor instance
	supervisorSpan trace.Span

	mu   sync.RWMutex
	cred *taskio.RunCredential
	// telemetryShutdown is the OTel provider shutdown the handshake's
	// telemetry.Setup returned (nil until then); owned by runSupervisor's
	// exit path, stored here because Setup runs inside awaitCredential.
	telemetryShutdown telemetry.ShutdownFunc
	// credAt is when the handshake completed (observability).
	credAt time.Time
	// traceCtx is the carrier-extracted context from the handshake
	// (ISI-4238): the per-task SubmitTask hands it to the engine so the
	// run's spans parent onto the Run's distributed trace. It is never a
	// cancellation source. Nil until the handshake lands.
	traceCtx context.Context
	// engine is the lazily-built runtime engine (nil until a /task arrives;
	// a warm unbound pod never pays the runtime construction cost).
	engine     *shim.Engine
	engineOnce sync.Once
	engineErr  error
	// busy guards the one-task-at-a-time D1 contract (one pod = one Run; a
	// concurrent POST is a dispatcher bug and gets a 409, never interleaved
	// event streams).
	busy bool
}

// awaitCredential polls the coord mount until the task-io credential lands,
// then completes the Bind→pod handshake: records the credential, Extracts the
// W3C carrier it carries, and initializes the telemetry spine on that trace
// context so supervisor + runtime spans join the Run's distributed trace.
func (s *supervisor) awaitCredential(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		cred, ok, err := taskio.ReadCredentialFromDir(supervisorCoordMountPath)
		if err != nil || !ok {
			continue // not yet: pre-Bind warm pod or mid-sync partial set — keep waiting
		}
		carrier := map[string]string{}
		if cred.TraceParent != "" {
			carrier["traceparent"] = cred.TraceParent
		}
		if cred.TraceState != "" {
			carrier["tracestate"] = cred.TraceState
		}
		tctx := telemetry.Extract(ctx, carrier)
		supTelemetryOpts := telemetry.Options{
			ServiceName: "ksquad-supervisor",
			Writer:      os.Stderr, // never stdout: /task's body IS the event wire
			// ISI-4413: the run's spans continue the operator-injected credential
			// traceparent; a sandbox hosts exactly one Run, so its run trace must
			// be captured even when that injected parent carries sampled=0 (which
			// would otherwise head-drop run.start/llm.call/run.end in the sandbox).
			CaptureUnsampledRemoteParent: true,
		} // M1.2 telemetry leg: the operator stamps OTEL_EXPORTER_OTLP_* (the
		// observability gateway) onto the sandbox pod env (warmpool
		// WithPodEnv); honor it for every signal still on the stdout default
		// so supervisor/runtime spans reach the gateway instead of dying on
		// stderr.
		if filled := telemetry.ApplyEnvOTLPFallback(&supTelemetryOpts, telemetry.EnvSignalExport(os.Getenv)); len(filled) > 0 {
			fmt.Fprintf(os.Stderr, "shim supervisor: OTLP export via OTEL_EXPORTER_OTLP_* env for %v (endpoint=%s)\n",
				filled, os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"))
		}
		if _, shutdown, terr := telemetry.Setup(tctx, supTelemetryOpts); terr == nil {
			s.mu.Lock()
			s.telemetryShutdown = shutdown
			s.mu.Unlock()
		} else {
			// A failed Setup leaves the global providers non-exporting (a partial
			// install may even shut the trace provider down), so the supervisor's
			// run spans would silently vanish — the exact ISI-4413 symptom. Never
			// swallow it: surface it on stderr so a dead spine is diagnosable
			// instead of masquerading as "OTLP export engaged".
			fmt.Fprintf(os.Stderr, "shim supervisor: telemetry.Setup failed, run spans will not export: %v\n", terr)
		}
		toolusage.SetEnabled(s.toolUsage)

		s.mu.Lock()
		s.cred = &cred
		s.credAt = time.Now().UTC()
		s.traceCtx = tctx // ISI-4238: span parent for the /task SubmitTask
		s.mu.Unlock()
		fmt.Fprintf(os.Stderr, "shim supervisor: handshake complete (run=%s work_item=%s)\n",
			shortID(cred.RunID), shortID(cred.WorkItemID))
		return
	}
}

// credential snapshots the handshake state.
func (s *supervisor) credential() (taskio.RunCredential, time.Time, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.cred == nil {
		return taskio.RunCredential{}, time.Time{}, false
	}
	return *s.cred, s.credAt, true
}

// teardownTelemetry snapshots the OTel shutdown for runSupervisor's exit
// path (nil before the handshake initialized the spine).
func (s *supervisor) teardownTelemetry() telemetry.ShutdownFunc {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.telemetryShutdown
}

// handleHealth is the liveness probe: the process is serving.
func (s *supervisor) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

// handleReady is the readiness probe. It reports Ready as soon as the server
// serves — readiness is deliberately NOT gated on the credential: an unbound
// warm pool pod must be Ready to be claimable, and the ADR-0007 handshake has
// its own explicit endpoint. The credential state rides the body so operators
// can see both levels in one place.
func (s *supervisor) handleReady(w http.ResponseWriter, _ *http.Request) {
	_, at, ok := s.credential()
	body := map[string]any{"serving": true, "credential": ok}
	if ok {
		body["credentialAt"] = at.UTC().Format(time.RFC3339)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(body)
}

// handleHandshake is the explicit Bind→pod handshake surface (M1.2 AC):
// 200 + the parsed credential's non-secret summary once the task-io files
// landed; 503 with progress detail before.
func (s *supervisor) handleHandshake(w http.ResponseWriter, _ *http.Request) {
	cred, at, ok := s.credential()
	if !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"handshake": false,
			"detail":    fmt.Sprintf("waiting for task-io credential files under %s", taskio.CoordMountPath),
		})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"handshake":    true,
		"runId":        cred.RunID,
		"workItemId":   cred.WorkItemID,
		"coordUrl":     cred.CoordURL,
		"traced":       cred.TraceParent != "",
		"credentialAt": at.UTC().Format(time.RFC3339),
	})
}

// handleTask is the D1 task-envelope endpoint: one a2a.Task JSON in, the
// JSONL run-event sequence out (chunked, flushed per event) — the same event
// framing `shim run` streams on stdout, so the operator-side bridge can reuse
// the a2a wire contract verbatim. 409 while another task is in flight (one
// pod = one Run), 500 when no runtime flavor was baked into the image.
func (s *supervisor) handleTask(w http.ResponseWriter, r *http.Request) {
	// Create a span for task handling
	ctx := r.Context()
	if s.supervisorSpan != nil {
		ctx = trace.ContextWithSpan(ctx, s.supervisorSpan)
	}

	ctx, taskHandleSpan := telemetry.Tracer().Start(ctx, "supervisor.handle_task",
		trace.WithAttributes(
			attribute.String("ksquad.task.id", r.URL.Query().Get("taskid")),
			attribute.String("ksquad.pod.name", os.Getenv("HOSTNAME")),
		))
	defer taskHandleSpan.End()

	s.mu.Lock()
	if s.busy {
		s.mu.Unlock()
		taskHandleSpan.SetStatus(codes.Error, "task already in flight")
		http.Error(w, "task in flight (one pod = one run)", http.StatusConflict)
		return
	}
	s.busy = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.busy = false
		s.mu.Unlock()
	}()

	engine, err := s.runtime()
	if err != nil {
		taskHandleSpan.SetStatus(codes.Error, fmt.Sprintf("runtime initialization failed: %v", err))
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var task a2a.Task
	if err := json.NewDecoder(r.Body).Decode(&task); err != nil {
		taskHandleSpan.SetStatus(codes.Error, fmt.Sprintf("decode task: %v", err))
		http.Error(w, fmt.Sprintf("decode task: %v", err), http.StatusBadRequest)
		return
	}

	// Add task correlation attributes
	taskHandleSpan.SetAttributes(
		attribute.String("ksquad.task.a2a_id", task.A2ATaskID),
		attribute.String("ksquad.task.work_item_id", task.WorkItemID),
		attribute.String("ksquad.task.model_route.endpoint", task.ModelRoute.Endpoint),
		attribute.String("ksquad.task.model_route.model", task.ModelRoute.Model),
	)

	// ISI-4238: submit on the handshake's trace context when it landed so
	// the run's spans join the Run's distributed trace; the request ctx
	// (streaming lifetime) stays with the stream below.
	submitCtx := r.Context()
	s.mu.RLock()
	if s.traceCtx != nil {
		submitCtx = s.traceCtx
	}
	s.mu.RUnlock()

	if _, err := engine.SubmitTask(submitCtx, task); err != nil {
		taskHandleSpan.SetStatus(codes.Error, fmt.Sprintf("submit task: %v", err))
		http.Error(w, fmt.Sprintf("submit: %v", err), http.StatusInternalServerError)
		return
	}

	// Create a span for event streaming
	_, streamSpan := telemetry.Tracer().Start(ctx, "supervisor.stream_events",
		trace.WithAttributes(
			attribute.String("ksquad.task.a2a_id", task.A2ATaskID),
		))
	defer streamSpan.End()

	events, err := engine.StreamEvents(r.Context(), task.A2ATaskID, 0)
	if err != nil {
		streamSpan.SetStatus(codes.Error, fmt.Sprintf("stream events: %v", err))
		http.Error(w, fmt.Sprintf("stream: %v", err), http.StatusInternalServerError)
		return
	}

	// One JSONL line per run event, flushed — the SSE-adjacent framing the
	// a2a contract already defines for `shim run`'s stdout.
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	enc := json.NewEncoder(w)

	// Mark task as successfully submitted
	taskHandleSpan.SetStatus(codes.Ok, "")

	for ev := range events {
		if err := enc.Encode(ev); err != nil {
			streamSpan.SetStatus(codes.Error, fmt.Sprintf("encode event: %v", err))
			return
		}
		if flusher != nil {
			flusher.Flush()
		}
	}

	streamSpan.SetStatus(codes.Ok, "")
}

// runtime lazily constructs the engine from the image's baked-in flavor
// (Dockerfile.shim ENV KSQUAD_RUNTIME_TYPE) plus the pod env Boot stamped.
func (s *supervisor) runtime() (*shim.Engine, error) {
	s.engineOnce.Do(func() {
		// Create a span for runtime initialization
		ctx := context.Background()
		if s.supervisorSpan != nil {
			ctx = trace.ContextWithSpan(context.Background(), s.supervisorSpan)
		}

		_, runtimeInitSpan := telemetry.Tracer().Start(ctx, "supervisor.runtime.init",
			trace.WithAttributes(
				attribute.String("ksquad.runtime.type", env("KSQUAD_RUNTIME_TYPE", os.Getenv("RUNTIME"))),
				attribute.String("ksquad.pod.name", os.Getenv("HOSTNAME")),
			))
		defer runtimeInitSpan.End()

		runtimeType := env("KSQUAD_RUNTIME_TYPE", os.Getenv("RUNTIME"))
		if runtimeType == "" {
			s.engineErr = fmt.Errorf("no runtime selected: image lacks KSQUAD_RUNTIME_TYPE")
			runtimeInitSpan.SetStatus(codes.Error, "no runtime selected")
			return
		}

		rt, err := runtimes.Get(runtimeType)
		if err != nil {
			s.engineErr = err
			runtimeInitSpan.SetStatus(codes.Error, fmt.Sprintf("runtime.Get failed: %v", err))
			runtimeInitSpan.RecordError(err)
			return
		}

		cfg, err := configFromEnv()
		if err != nil {
			s.engineErr = err
			runtimeInitSpan.SetStatus(codes.Error, fmt.Sprintf("configFromEnv failed: %v", err))
			runtimeInitSpan.RecordError(err)
			return
		}

		engine := shim.New(rt, shim.NewOSRunner(), cfg)
		// ISI-4385: register the tool-usage metric set on the supervisor's real
		// registry (was nil → tool/skill/llm metrics never registered in
		// supervisor-mode pods) so ksquad_tool_calls_total, ksquad_skill_loads_total,
		// ksquad_mcp_call_duration_seconds and the llm counters flow from the pod
		// via GET /metrics.
		engine.SetTelemetry(toolusage.NewMapper(telemetry.Tracer(), s.metricsReg))

		// Mark runtime initialization as successful
		runtimeInitSpan.SetStatus(codes.Ok, "")
		s.engine = engine
	})
	return s.engine, s.engineErr
}

// shortID renders a uid for logs without leaking the full identifier into
// stderr noise (they are not secrets, just long).
func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8] + "…" + strconv.Itoa(len(id))
}
