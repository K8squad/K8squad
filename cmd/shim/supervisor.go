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

	sup := &supervisor{
		// toolusage gate mirrors `shim run` (default on; Boot stamps the
		// operator's OTelConfig-derived toggle into the sandbox env).
		toolUsage: env("KSQUAD_TOOL_USAGE_ENABLED", "true") != "false",
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", sup.handleHealth)
	mux.HandleFunc("GET /ready", sup.handleReady)
	mux.HandleFunc("GET /handshake", sup.handleHandshake)
	mux.HandleFunc("POST /task", sup.handleTask)

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

	mu   sync.RWMutex
	cred *taskio.RunCredential
	// telemetryShutdown is the OTel provider shutdown the handshake's
	// telemetry.Setup returned (nil until then); owned by runSupervisor's
	// exit path, stored here because Setup runs inside awaitCredential.
	telemetryShutdown telemetry.ShutdownFunc
	// credAt is when the handshake completed (observability).
	credAt time.Time
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
		}
		// M1.2 telemetry leg: the operator stamps OTEL_EXPORTER_OTLP_* (the
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
		}
		toolusage.SetEnabled(s.toolUsage)

		s.mu.Lock()
		s.cred = &cred
		s.credAt = time.Now().UTC()
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
		"handshake":   true,
		"runId":       cred.RunID,
		"workItemId":  cred.WorkItemID,
		"coordUrl":    cred.CoordURL,
		"traced":      cred.TraceParent != "",
		"credentialAt": at.UTC().Format(time.RFC3339),
	})
}

// handleTask is the D1 task-envelope endpoint: one a2a.Task JSON in, the
// JSONL run-event sequence out (chunked, flushed per event) — the same event
// framing `shim run` streams on stdout, so the operator-side bridge can reuse
// the a2a wire contract verbatim. 409 while another task is in flight (one
// pod = one Run), 500 when no runtime flavor was baked into the image.
func (s *supervisor) handleTask(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	if s.busy {
		s.mu.Unlock()
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
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var task a2a.Task
	if err := json.NewDecoder(r.Body).Decode(&task); err != nil {
		http.Error(w, fmt.Sprintf("decode task: %v", err), http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	if _, err := engine.SubmitTask(ctx, task); err != nil {
		http.Error(w, fmt.Sprintf("submit: %v", err), http.StatusInternalServerError)
		return
	}
	events, err := engine.StreamEvents(ctx, task.A2ATaskID, 0)
	if err != nil {
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
	for ev := range events {
		if err := enc.Encode(ev); err != nil {
			return
		}
		if flusher != nil {
			flusher.Flush()
		}
	}
}

// runtime lazily constructs the engine from the image's baked-in flavor
// (Dockerfile.shim ENV KSQUAD_RUNTIME_TYPE) plus the pod env Boot stamped.
func (s *supervisor) runtime() (*shim.Engine, error) {
	s.engineOnce.Do(func() {
		runtimeType := env("KSQUAD_RUNTIME_TYPE", os.Getenv("RUNTIME"))
		if runtimeType == "" {
			s.engineErr = fmt.Errorf("no runtime selected: image lacks KSQUAD_RUNTIME_TYPE")
			return
		}
		rt, err := runtimes.Get(runtimeType)
		if err != nil {
			s.engineErr = err
			return
		}
		cfg, err := configFromEnv()
		if err != nil {
			s.engineErr = err
			return
		}
		engine := shim.New(rt, shim.NewOSRunner(), cfg)
		engine.SetTelemetry(toolusage.NewMapper(telemetry.Tracer(), nil))
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
