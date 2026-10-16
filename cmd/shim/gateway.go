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

// gateway.go — the AgentMesh pool-pod Gateway (chapter §2.3 D1.2, ISI-5822
// P1.1): the evolved `shim supervisor`, standing beside it rather than
// replacing it (additive-only, D9.3 — the legacy supervisor and its claim
// path stay byte-identical). When the sandbox pod's entrypoint runs
// `shim gateway`, this process is the pod's PID 1 and:
//
//	spawns Min standing role-processes (`shim supervisor` children, each
//	           pre-loaded with the role envelope via the RuntimeAdapter —
//	           opencode behind the seam, D8.1) on 127.0.0.1:<base>+i,
//	registers their A2A endpoints with the Supervisor ingress
//	           (KSQUAD_SUPERVISOR_INGRESS_URL) on a 30s heartbeat that
//	           doubles as the coord.mesh_endpoint TTL refresh,
//	serves the kube contract on :8080 — the SAME ports and probe shape the
//	           warm-pool Boot spec already knows:
//
//	GET  /health      liveness — the gateway process is serving.
//	GET  /ready       readiness — the pool holds >= 1 standing process.
//	GET  /endpoints   the live endpoint set (id, endpoint, busy) — the
//	                 operator-side debug/read surface.
//	POST /task        route ONE A2A task to a standing process. The target
//	                 process is the ?target=<endpoint_id> query parameter;
//	                 an omitted target picks an idle process. 409 when the
//	                 target is busy or the pool is saturated (D3.3 guard
//	                 #3, the gateway-local one-process-one-active-task).
//
// The gateway routes; it never claims. Custody stays in coord.claim and
// at-most-once stays in coord.a2a_dispatch on the operator side — a routed
// task reaches the SAME /task contract the legacy supervisor serves, so
// both paths terminate at the identical idempotency marker (§5.4).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/K8squad/K8squad/pkg/a2a"
	"github.com/K8squad/K8squad/pkg/mesh"
	"github.com/K8squad/K8squad/pkg/telemetry"
	"github.com/K8squad/K8squad/pkg/telemetry/meshobs"
)

// gatewayAddr is the gateway's own listen address — the :8080 kube contract
// (the warm-pool Boot probe), overridable for tests and non-kube dev runs.
const gatewayAddr = ":8080"

// gatewayChildBasePort is the first port the pool's standing children bind
// (LocalHost assigns base+i). The children sit ABOVE the gateway's own
// :8080 so the pod's probe surface never collides with a role process.
const gatewayChildBasePort = 8081

// runGateway is the `shim gateway` entrypoint. Like runSupervisor it never
// returns nil on server failure — a gateway that cannot serve is a dead
// pool pod, and CrashLoopBackOff is honest degradation.
func runGateway(args []string) error {
	addr := gatewayAddr
	if v := os.Getenv("KSQUAD_SUPERVISOR_ADDR"); v != "" {
		addr = v
	}
	if len(args) > 0 && args[0] != "" && os.Getenv("KSQUAD_SUPERVISOR_ADDR") == "" {
		addr = args[0]
	}

	// Same telemetry spine discipline as the supervisor (ISI-5142): the
	// OTLP exporter comes up at process start, before any span, so the
	// ksquad-gateway service entity registers even while the pool boots.
	gwTelemetryOpts, filled := supervisorTelemetryOptions(os.Getenv)
	gwTelemetryOpts.ServiceName = "ksquad-gateway"
	if len(filled) > 0 {
		fmt.Fprintf(os.Stderr, "shim gateway: OTLP export via OTEL_EXPORTER_OTLP_* env for %v (endpoint=%s)\n",
			filled, os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"))
	} else {
		fmt.Fprintln(os.Stderr, "shim gateway: OTEL_EXPORTER_OTLP_ENDPOINT unset — "+
			"spans stay on stderr and will NOT reach the observability gateway (no ksquad-gateway service entity)")
	}
	if _, shutdown, terr := telemetry.Setup(context.Background(), gwTelemetryOpts); terr == nil {
		defer func() { _ = shutdown(context.Background()) }()
	} else {
		fmt.Fprintf(os.Stderr, "shim gateway: telemetry.Setup failed, spans will not export: %v\n", terr)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	_, gwSpan := telemetry.Tracer().Start(ctx, "gateway.start",
		trace.WithAttributes(
			attribute.String("code.namespace", "github.com/K8squad/K8squad/cmd/shim"),
			attribute.String("code.function", "runGateway"),
			attribute.String("ksquad.supervisor.addr", addr),
			attribute.String("ksquad.pod.name", os.Getenv("HOSTNAME")),
			attribute.String("ksquad.mesh.team", os.Getenv("KSQUAD_MESH_TEAM")),
			attribute.String("ksquad.mesh.role", os.Getenv("KSQUAD_MESH_ROLE")),
		))
	defer gwSpan.End()

	gw, err := buildGateway()
	if err != nil {
		gwSpan.SetStatus(1, err.Error())
		return err
	}
	if err := gw.Start(ctx); err != nil {
		gwSpan.SetStatus(1, err.Error())
		return fmt.Errorf("shim gateway: start pool: %w", err)
	}
	defer func() { _ = gw.Stop(context.Background()) }()

	// The mesh lifecycle telemetry plug (P1.2, ISI-5823): the processes.active
	// gauge reads the pool's live slot snapshot on every collection cycle
	// (mesh.route/spawn spans + the latency instruments ride the Observer the
	// gateway was built with — set before Start so even the cold-boot spawns
	// are observed).
	team, role := os.Getenv("KSQUAD_MESH_TEAM"), os.Getenv("KSQUAD_MESH_ROLE")
	if reg, err := meshobs.RegisterProcessesGauge(telemetry.Meter(), team, role, func() map[mesh.ProcState]int64 {
		counts := map[mesh.ProcState]int64{}
		for _, p := range gw.Processes() {
			counts[p.State]++
		}
		return counts
	}); err == nil && reg != nil {
		defer func() { _ = reg.Unregister() }()
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /ready", handleGatewayReady(gw))
	mux.HandleFunc("GET /endpoints", handleGatewayEndpoints(gw))
	mux.HandleFunc("POST /task", handleGatewayTask(gw))

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	fmt.Fprintf(os.Stderr, "shim gateway: serving on %s (pool team=%s role=%s min=%d max=%d)\n",
		addr, os.Getenv("KSQUAD_MESH_TEAM"), os.Getenv("KSQUAD_MESH_ROLE"), gatewayMin(), gatewayMax())

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("gateway server: %w", err)
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

// gatewayMin reads the standing-process floor (KSQUAD_MESH_MIN; P1.1 default
// 1 — one team, one coder pool, min:1 always-up).
func gatewayMin() int {
	if v := os.Getenv("KSQUAD_MESH_MIN"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 1
}

// gatewayMax reads the standing-process saturation ceiling (KSQUAD_MESH_MAX,
// P1.3 ISI-5824). 0 or <= min means "no growth band": the pool keeps the
// P1.2 single-floor shape (saturation refuses; no grow, no overflow).
func gatewayMax() int {
	if v := os.Getenv("KSQUAD_MESH_MAX"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 0
}

// buildGateway assembles the mesh.Gateway from the pod-spec env the operator
// stamped at Boot (D9.2 envelope pre-load): team/role/identity, model,
// skills, pool floor, and the Supervisor registration ingress. The adapter
// is bound from the pool's declared runtime type (KSQUAD_RUNTIME_TYPE, the
// RolePool spec.runtimeRef resolution — the P2 close-out follow-up, ISI-5826
// comment b09e1970: composition-root binding, additive, outside the pkg/mesh
// seam) on the LocalHost child-process substrate; a kube host replaces it
// without the gateway changing (that substitution is the P2 seam proof).
func buildGateway() (*mesh.Gateway, error) {
	team := os.Getenv("KSQUAD_MESH_TEAM")
	role := os.Getenv("KSQUAD_MESH_ROLE")
	if team == "" || role == "" {
		return nil, fmt.Errorf("shim gateway: KSQUAD_MESH_TEAM and KSQUAD_MESH_ROLE are required (the pool identity)")
	}
	base := gatewayChildBasePort
	if v := os.Getenv("KSQUAD_MESH_BASE_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			base = n
		}
	}
	host := &mesh.LocalHost{BasePort: base}
	runtimeType := env("KSQUAD_RUNTIME_TYPE", "opencode")
	adapter, err := buildRuntimeAdapter(runtimeType, host)
	if err != nil {
		return nil, fmt.Errorf("shim gateway: %s adapter: %w", runtimeType, err)
	}
	identity := a2a.AgentIdentity{
		Name:    env("KSQUAD_MESH_AGENT_NAME", role),
		Squad:   team,
		Project: os.Getenv("KSQUAD_MESH_PROJECT"),
	}
	pod := env("KSQUAD_MESH_POD", os.Getenv("HOSTNAME"))
	// The lifecycle observer rides the OTLP spine the gateway process already
	// exports through (telemetry.Tracer/Meter are safe pre-Setup: no-ops).
	observer := meshobs.New(telemetry.Tracer(), telemetry.Meter(), meshobs.Labels{Team: team, Role: role, Pod: pod})
	return mesh.NewGateway(mesh.GatewayConfig{
		Adapter: adapter,
		Envelope: mesh.Envelope{
			RuntimeType: runtimeType,
			Model:       os.Getenv("KSQUAD_MESH_MODEL"),
			Skills:      splitList(os.Getenv("KSQUAD_MESH_SKILLS")),
			Identity:    identity,
			// Stamp the S0/S3 dispatch-path label at the source (ISI-5833 I6
			// + ISI-5822): a mesh standing process only ever executes
			// mesh-routed runs, so its run.start roots carry run.path=mesh.
			Env: map[string]string{"KSQUAD_RUN_PATH": "mesh"},
		},
		Min:              gatewayMin(),
		Max:              gatewayMax(),
		Pod:              pod,
		Team:             team,
		Role:             role,
		RegisterURL:      strings.TrimRight(os.Getenv("KSQUAD_SUPERVISOR_INGRESS_URL"), "/"),
		RegisterInterval: gatewayRegisterIntervalFromEnv(),
		// The routable endpoint is the gateway's own :8080 (§5.2): children
		// bind 127.0.0.1 inside the pod, unreachable from the operator.
		// KSQUAD_MESH_ADVERTISE_URL overrides; the default derives from the
		// downward-API POD_IP the mesh pool pod spec stamps.
		AdvertiseURL: gatewayAdvertiseURL(),
		// P1.2 lifecycle knobs (D4.2): KSQUAD_MESH_LIFECYCLE_* env the
		// operator stamps per pool; unset fields keep the defaults (30s
		// check / 5m idleToHibernate / 4h hibernateToStop / alwaysUp=false).
		// The gateway owns the clock. KSQUAD_MESH_LIFECYCLE_HIBERNATE=true
		// arms the S4 two-tier ladder (ISI-5831); absent keeps the collapsed
		// P1.2 ladder — hibernation rolls out pool-by-pool, reversibly.
		Lifecycle: gatewayLifecycleFromEnv(),
		Observer:  observer,
	})
}

// buildRuntimeAdapter binds the pool's declared runtime type to its
// RuntimeAdapter at the composition root: the gateway itself never picks a
// runtime — the pod's stamped type decides (opencode #1, codex #2, chapter
// §1B/§9.3). An untyped pod keeps "opencode", exactly what every mesh pool
// ran before this seam existed; an unknown type fails closed — a
// wrong-shaped pool must crash honestly, never silently run opencode
// under another runtime's image.
func buildRuntimeAdapter(runtimeType string, host mesh.PlatformHost) (mesh.RuntimeAdapter, error) {
	switch runtimeType {
	case "opencode":
		return mesh.NewOpenCodeAdapter(host)
	case "codex":
		return mesh.NewCodexAdapter(host)
	default:
		return nil, fmt.Errorf("no RuntimeAdapter bound for runtime type %q (have: opencode, codex)", runtimeType)
	}
}

// gatewayLifecycleFromEnv reads the per-pool lifecycle knobs (chapter §6.2
// D4.2) from the pod-spec env. Malformed durations fall back to the operator
// default rather than killing the pool pod — a bad knob must degrade to the
// default cadence, not to CrashLoopBackOff.
func gatewayLifecycleFromEnv() mesh.LifecycleConfig {
	return mesh.LifecycleConfig{
		CheckInterval:   envDuration("KSQUAD_MESH_LIFECYCLE_CHECK_INTERVAL"),
		Hibernate:       os.Getenv("KSQUAD_MESH_LIFECYCLE_HIBERNATE") == "true",
		IdleToHibernate: envDuration("KSQUAD_MESH_LIFECYCLE_IDLE_TO_HIBERNATE"),
		HibernateToStop: envDuration("KSQUAD_MESH_LIFECYCLE_HIBERNATE_TO_STOP"),
		AlwaysUp:        os.Getenv("KSQUAD_MESH_LIFECYCLE_ALWAYS_UP") == "true",
	}
}

// envDuration parses one duration knob; empty or malformed yields 0 (the
// gateway default).
func envDuration(key string) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0
	}
	return d
}

// gatewayAdvertiseURL resolves the URL the registry advertises for this
// gateway's processes: an explicit KSQUAD_MESH_ADVERTISE_URL override, else
// the pod IP the kubelet injected via the status.podIP downward-API env,
// else "" (unroutable — local/dev lanes keep the child endpoints).
func gatewayAdvertiseURL() string {
	if v := os.Getenv("KSQUAD_MESH_ADVERTISE_URL"); v != "" {
		return strings.TrimRight(v, "/")
	}
	if ip := os.Getenv("POD_IP"); ip != "" {
		return "http://" + net.JoinHostPort(ip, "8080")
	}
	return ""
}

// gatewayRegisterIntervalFromEnv reads the heartbeat cadence override.
func gatewayRegisterIntervalFromEnv() time.Duration {
	if v := os.Getenv("KSQUAD_SUPERVISOR_INGRESS_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return 0 // mesh.Gateway default (30s)
}

// handleGatewayReady is the pool-pod readiness gate: Ready iff the pool can
// SERVE — a standing process OR a stopped (recreate-on-demand) slot. A pool
// that legitimately scaled its processes to zero overnight must NOT flip
// NotReady: that would cycle the pod and defeat the reclaim (§6.1). The body
// surfaces the state counts so an operator sees scale-to-zero at a glance.
func handleGatewayReady(gw *mesh.Gateway) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		procs := gw.Processes()
		counts := map[string]int{}
		for _, p := range procs {
			counts[string(p.State)]++
		}
		w.Header().Set("Content-Type", "application/json")
		if !gw.Ready() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]any{"serving": true, "processes": 0, "ready": false})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"serving":   true,
			"processes": len(procs),
			"states":    counts,
			"ready":     true,
		})
	}
}

// handleGatewayEndpoints serves the live endpoint set — the operator-side
// read surface for debugging routing decisions.
func handleGatewayEndpoints(gw *mesh.Gateway) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"processes": gw.Processes()})
	}
}

// handleGatewayTask routes ONE A2A task to a standing process: the
// Supervisor-side dispatch POST (the same JSON-in / NDJSON-out wire the
// legacy supervisor serves, so the operator's HTTP transport bridges to
// either unchanged). ?target=<endpoint_id> names the process; omitted picks
// an idle one. 409 on a busy target or a saturated pool — never interleaved
// event streams (D3.3 guard #3).
func handleGatewayTask(gw *mesh.Gateway) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := telemetry.Tracer().Start(r.Context(), "gateway.handle_task",
			trace.WithAttributes(
				attribute.String("code.namespace", "github.com/K8squad/K8squad/cmd/shim"),
				attribute.String("code.function", "handleGatewayTask"),
				attribute.String("ksquad.task.target", r.URL.Query().Get("target")),
				attribute.String("ksquad.pod.name", os.Getenv("HOSTNAME")),
			),
			trace.WithSpanKind(trace.SpanKindServer))
		defer span.End()

		var task a2a.Task
		if err := json.NewDecoder(r.Body).Decode(&task); err != nil {
			span.SetStatus(1, err.Error())
			http.Error(w, fmt.Sprintf("decode task: %v", err), http.StatusBadRequest)
			return
		}
		span.SetAttributes(attribute.String("ksquad.task.a2a_id", task.A2ATaskID))

		stream, err := gw.Route(ctx, r.URL.Query().Get("target"), task)
		if err != nil {
			switch {
			case errors.Is(err, mesh.ErrProcessBusy), errors.Is(err, mesh.ErrNoIdleProcess):
				span.SetStatus(1, err.Error())
				http.Error(w, err.Error(), http.StatusConflict)
			default:
				span.SetStatus(1, err.Error())
				http.Error(w, err.Error(), http.StatusBadGateway)
			}
			return
		}
		defer stream.Close()
		// A dropped follower must release the process: bind the routed
		// stream's lifetime to the REQUEST context, so the client going
		// away tears the stream (and its busy flag) down instead of
		// leaking a busy process behind a dead connection. The deferred
		// Close is idempotent with this one.
		go func() {
			<-ctx.Done()
			_ = stream.Close()
		}()

		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		enc := json.NewEncoder(w)
		span.SetStatus(0, "")
		for ev := range stream.Events() {
			if err := enc.Encode(ev); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
}
