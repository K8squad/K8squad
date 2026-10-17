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

// meshe2e_test.go — the AgentMesh P1.1 thin-thread route-through proof
// (chapter §5.2, ISI-5822 slice 4): one run routed end-to-end over the REAL
// components — the mesh.Gateway (registration + /task routing), the
// rundrive.MeshIngress register surface, and the coord.MeshRegistry lookup —
// with the task POSTed through the SAME a2a.HTTPTransport client the
// operator's route leg uses (dispatch.go's sandboxTransport mesh branch).
//
// Fakes are exactly where the chapter draws the seams: the RuntimeAdapter is
// scripted (opencode is adapter #1 behind the seam, D8.1) and Postgres is
// sqlmock (the registry's SQL is pinned by pkg/coord's own tests). Everything
// between — the registration wire, the ingress decode, the registry rows, the
// lookup ordering, the advertise rewrite, the /task NDJSON contract — is the
// production code path.
//
// What this pins that the slice tests could not: the COMPOSITION. The
// gateway's Registration JSON is decoded by the real ingress; the advertised
// endpoint the registry stores is the GATEWAY's routable URL (AdvertiseURL —
// children bind 127.0.0.1 in-pod only); the URL the route leg would resolve
// (eps[0].Endpoint + "/task") actually serves the gateway's /task contract;
// and the a2a.HTTPTransport round-trips the gateway's NDJSON stream.
package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"
	"github.com/prometheus/client_golang/prometheus"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"

	clienta2a "github.com/K8squad/K8squad/internal/a2a"
	"github.com/K8squad/K8squad/pkg/a2a"
	"github.com/K8squad/K8squad/pkg/controller/rundrive"
	"github.com/K8squad/K8squad/pkg/coord"
	"github.com/K8squad/K8squad/pkg/mesh"
	"github.com/K8squad/K8squad/pkg/telemetry"
)

// debugSink surfaces registration-loop failures the gateway swallows by
// design (the next heartbeat retries) straight into the test log.
type debugSink struct{ t *testing.T }

func (s debugSink) OnLifecycle(op, handleID, endpoint string, err error) {
	if err != nil {
		s.t.Logf("gateway lifecycle %s: %v", op, err)
	}
}

// TestMeshThinThread_RouteThrough walks §5.2's dispatch sequence minus the
// board custody legs (coord.claim / coord.a2a_dispatch are unchanged by P1.1
// and pinned by their own tests + the FR-B3 contract):
//
//	gateway Start → RegisterOnce → ingress /register → registry upsert+prune
//	→ LookupTeamRole(team, role) → HTTPTransport POST <endpoint>/task
//	→ gateway routes to the standing process → NDJSON events settle the session.
func TestMeshThinThread_RouteThrough(t *testing.T) {
	const (
		team = "team-acme"
		role = "coder"
		pod  = "mesh-team-acme-coder"
	)
	ctx := context.Background()

	// The authoritative registry store (sqlmock stands in for Postgres).
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	registry, err := coord.NewMeshRegistry(db)
	if err != nil {
		t.Fatal(err)
	}

	// The Supervisor-side register ingress: the real handler surface the
	// gateway heartbeats at. (Mounted at the root — ServeMeshIngress's
	// /mesh/ prefixing has its own coverage; StripPrefix around a
	// method-pattern mux breaks Go's ServeMux matching, so keep it bare.)
	in, err := rundrive.NewMeshIngress(registry)
	if err != nil {
		t.Fatal(err)
	}
	ingress := httptest.NewServer(in.Handler())
	t.Cleanup(ingress.Close)

	// Pre-bind the gateway's :8080-equivalent listener so the ADVERTISED url
	// (what the registry will serve the route leg) is known before Start.
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gwURL := "http://" + lis.Addr().String()

	// The immediate registration beat Start fires: one upsert per process
	// with the ADVERTISED url (not the child's 127.0.0.1), then the prune.
	mock.ExpectExec("INSERT INTO coord.mesh_endpoint").
		WithArgs("fake-0", team, role, pod, gwURL, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("DELETE FROM coord.mesh_endpoint").
		WithArgs(pod, pq.Array([]string{"fake-0"})).
		WillReturnResult(sqlmock.NewResult(0, 0))

	fa := &fakeGwAdapter{autoSettle: true}
	gw, err := mesh.NewGateway(mesh.GatewayConfig{
		Adapter:     fa,
		Envelope:    mesh.Envelope{RuntimeType: "opencode"},
		Min:         1,
		Pod:         pod,
		Team:        team,
		Role:        role,
		RegisterURL: ingress.URL,
		// §5.2: the route target is gateway(target.pod) — the child's
		// loopback endpoint is unreachable from the operator side.
		AdvertiseURL: gwURL,
		Sink:         debugSink{t},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := gw.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = gw.Stop(ctx) })

	// The gateway's kube-contract HTTP surface on the pre-bound listener —
	// the same mux runGateway serves.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /ready", handleGatewayReady(gw))
	mux.HandleFunc("GET /endpoints", handleGatewayEndpoints(gw))
	mux.HandleFunc("POST /task", handleGatewayTask(gw))
	gwSrv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = gwSrv.Serve(lis) }()
	t.Cleanup(func() { _ = gwSrv.Close() })

	// The route leg's read: the registry returns the advertised endpoint.
	// (Ordered-expectation note: this SELECT is queued AFTER the register
	// beat's INSERT+prune above — sqlmock pins the sequence.)
	mock.ExpectQuery("FROM coord.mesh_endpoint").
		WithArgs(team, role, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{
			"endpoint_id", "team", "role", "pod", "endpoint", "agent_card", "registered_at", "last_seen_at",
		}).AddRow("fake-0", team, role, pod, gwURL, []byte(`{}`),
			time.Now().Add(-time.Hour), time.Now().Add(-time.Second)))
	eps, err := registry.LookupTeamRole(ctx, team, role)
	if err != nil {
		t.Fatalf("LookupTeamRole: %v", err)
	}
	if len(eps) != 1 {
		t.Fatalf("registry endpoints = %d, want 1", len(eps))
	}
	if eps[0].Endpoint != gwURL {
		t.Fatalf("registry endpoint = %q, want the ADVERTISED gateway url %q (a child loopback url is unroutable from the operator)", eps[0].Endpoint, gwURL)
	}

	// Submit through the SAME transport the route leg binds (dispatch.go:
	// a2a.HTTPTransport{URL: eps[0].Endpoint + "/task"}) — proving the
	// gateway's /task wire answers the operator's client contract.
	taskURL := eps[0].Endpoint + "/task"
	ht := &clienta2a.HTTPTransport{
		URL: func(context.Context, a2a.Task) (string, error) { return taskURL, nil },
	}
	const taskID = "dddddddd-0000-4444-8888-dddddddddddd"
	sess, err := ht.Submit(ctx, a2a.Task{
		A2ATaskID: taskID,
		Identity:  a2a.AgentIdentity{Squad: team, Name: role},
	})
	if err != nil {
		t.Fatalf("route-leg Submit: %v", err)
	}
	events := 0
	for range sess.Events() {
		events++
	}
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	if events == 0 {
		t.Fatal("routed task streamed zero events — the /task NDJSON contract did not round-trip")
	}

	// The standing process executed exactly once (guard #3's flip side:
	// one route, one execution — at-most-once stays in coord.a2a_dispatch,
	// unchanged and out of scope here by design).
	fa.mu.Lock()
	routes := len(fa.streams)
	fa.mu.Unlock()
	if routes != 1 {
		t.Fatalf("standing-process routes = %d, want 1", routes)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// routerGwAdapter is the trace-continuity proof's RuntimeAdapter (ISI-6062,
// ISI-6055 P2): the lifecycle ops are bookkeeping no-ops — the standing
// "process" is the in-process supervisor server below — but Route is the
// REAL production leg: the mesh httpRouter with the W3C carrier seam,
// wired exactly like runGateway (WithRouteHeaders + telemetry.Inject), so
// the routed child POST carries the gateway's trace context on its headers.
type routerGwAdapter struct {
	endpoint string
	router   mesh.Router
}

func (a *routerGwAdapter) Spawn(_ context.Context, _ mesh.Envelope) (mesh.Handle, a2a.AgentCard, error) {
	return &fakeGwHandle{id: "trace-0", ep: a.endpoint}, a2a.AgentCard{}, nil
}

func (a *routerGwAdapter) Register(_ context.Context, _ mesh.Handle) (a2a.AgentCard, error) {
	return a2a.AgentCard{}, nil
}

func (a *routerGwAdapter) Route(ctx context.Context, h mesh.Handle, task a2a.Task) (mesh.Stream, error) {
	return a.router.Route(ctx, h.Endpoint(), task)
}

func (a *routerGwAdapter) Hibernate(context.Context, mesh.Handle) error { return nil }
func (a *routerGwAdapter) Wake(context.Context, mesh.Handle) error      { return nil }
func (a *routerGwAdapter) Stop(context.Context, mesh.Handle) error      { return nil }

func (a *routerGwAdapter) Recreate(_ context.Context, h mesh.Handle, _ mesh.Envelope) (mesh.Handle, error) {
	return h, nil
}

var _ mesh.RuntimeAdapter = (*routerGwAdapter)(nil)

// stubsOfTrace returns the exporter's ended spans that landed on tid.
func stubsOfTrace(exp *tracetest.InMemoryExporter, tid oteltrace.TraceID) []tracetest.SpanStub {
	var out []tracetest.SpanStub
	for _, s := range exp.GetSpans() {
		if s.SpanContext.TraceID() == tid {
			out = append(out, s)
		}
	}
	return out
}

// stubNamed finds the first span with the given name among stubs.
func stubNamed(stubs []tracetest.SpanStub, name string) (tracetest.SpanStub, bool) {
	for _, s := range stubs {
		if s.Name == name {
			return s, true
		}
	}
	return tracetest.SpanStub{}, false
}

// stubAttr returns a span's string attribute, "" when absent.
func stubAttr(s tracetest.SpanStub, key string) string {
	for _, kv := range s.Attributes {
		if string(kv.Key) == key {
			return kv.Value.AsString()
		}
	}
	return ""
}

// TestMeshTraceContinuity_GatewayToSupervisor (ISI-6062, ISI-6055 P2 — plan
// §3 phase 4) pins the W3C continuity of ONE logical task across every mesh
// hop, over the REAL components end to end: the operator's a2a transport
// (HeaderFunc carrier injection, the dispatch.go seam) → the gateway's
// /task (Extract before gateway.handle_task) → the mesh httpRouter
// (WithRouteHeaders injection) → the REAL supervisor handleTask HTTP
// surface → a REAL engine run (run.start … run.end through the toolusage
// mapper). Only the runtime BINARY is faked: an `openclaw` stub on PATH
// prints one line and exits 0, so the engine drives a genuine subprocess
// run without a model behind it.
//
// The bluebox baseline this replaces: pre-fix, the same task fragmented
// into 3+ traces (b7eb5b4a…: gateway root, supervisor elsewhere; 0241773d…:
// every supervisor.handle_task on a warm pod sharing one dangling parent).
// Here gateway.handle_task, supervisor.handle_task, run.start, skill.load
// and run.end must share ONE trace id, every supervisor-side span's parent
// must resolve inside that trace (zero dangling parents), and run #2 on
// the SAME warm gateway + standing process must root its OWN trace — the
// warm-pod bleed regression guard (ISI-5331 R1 class).
func TestMeshTraceContinuity_GatewayToSupervisor(t *testing.T) {
	// The mesh-spawned "child" carries its runtime flavor by env; drive the
	// openclaw one so the engine's Command() resolves our PATH stub.
	t.Setenv("KSQUAD_RUNTIME_TYPE", "openclaw")
	// One granted skill: drive() emits a skill.load span per granted skill,
	// so the continuity proof also covers the plan's phase-3 detail span.
	t.Setenv("KSQUAD_SKILLS", "paperclip-workflow")
	binDir := t.TempDir()
	stub := filepath.Join(binDir, "openclaw")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\necho trace-proof-ok\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// Global OTel: in-memory exporter + the W3C TraceContext propagator —
	// the same globals telemetry.Tracer()/Inject/Extract ride in production.
	prevTP := otel.GetTracerProvider()
	prevProp := otel.GetTextMapPropagator()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
	})

	// The standing child process: the REAL supervisor handleTask surface.
	// No credential handshake ran, so its trace context can arrive ONLY
	// via the routed POST's W3C headers — exactly the mesh leg's contract.
	sup := &supervisor{metricsReg: prometheus.NewRegistry()}
	supMux := http.NewServeMux()
	supMux.HandleFunc("POST /task", sup.handleTask)
	supSrv := httptest.NewServer(supMux)
	t.Cleanup(supSrv.Close)

	// The gateway's route leg: the real httpRouter with the runGateway
	// carrier seam, so the child POST carries the gateway's trace context.
	router := mesh.NewHTTPRouter(nil, mesh.WithRouteHeaders(func(ctx context.Context) map[string]string {
		carrier := map[string]string{}
		telemetry.Inject(ctx, carrier)
		return carrier
	}))
	adapter := &routerGwAdapter{endpoint: supSrv.URL, router: router}
	gw, err := mesh.NewGateway(mesh.GatewayConfig{
		Adapter:  adapter,
		Envelope: mesh.Envelope{RuntimeType: "openclaw"},
		Min:      1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := gw.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = gw.Stop(context.Background()) })

	gwMux := http.NewServeMux()
	gwMux.HandleFunc("POST /task", handleGatewayTask(gw))
	gwSrv := httptest.NewServer(gwMux)
	t.Cleanup(gwSrv.Close)

	// submitRun is the operator leg: a fresh dispatch root span, the SAME
	// a2a.HTTPTransport + HeaderFunc carrier seam dispatch.go binds, Submit
	// through the gateway's /task, and a full NDJSON drain so the run
	// settles before the trace is read back.
	submitRun := func(taskID string) oteltrace.TraceID {
		ctx, opSpan := tp.Tracer("ksquad/operator").Start(context.Background(), "operator.dispatch")
		ht := &clienta2a.HTTPTransport{
			URL: func(context.Context, a2a.Task) (string, error) { return gwSrv.URL + "/task", nil },
			HeaderFunc: func(ctx context.Context) map[string]string {
				carrier := map[string]string{}
				telemetry.Inject(ctx, carrier)
				return carrier
			},
		}
		sess, err := ht.Submit(ctx, a2a.Task{
			A2ATaskID:  taskID,
			WorkItemID: "11111111-2222-4333-8444-555555555555",
			TicketKey:  "ISI-6062",
			Identity:   a2a.AgentIdentity{Squad: "team-acme", Name: "coder", Project: "proj-6062"},
		})
		if err != nil {
			t.Fatalf("operator Submit %s: %v", taskID, err)
		}
		events := 0
		for range sess.Events() {
			events++
		}
		if err := sess.Close(); err != nil {
			t.Fatalf("operator session close %s: %v", taskID, err)
		}
		if events == 0 {
			t.Fatalf("operator session %s streamed zero events — the run never settled", taskID)
		}
		opSpan.End()
		return opSpan.SpanContext().TraceID()
	}

	// waitIdle polls the gateway pool until the standing process releases —
	// run #2 must not race run #1's release into the 409 guard.
	waitIdle := func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			procs := gw.Processes()
			if len(procs) == 1 && !procs[0].Busy {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("standing process never released after run settled")
	}

	// waitRunEnd blocks until the run's run.end span landed on tid — drive()
	// ends the run root AFTER the wire stream closes, so the drained session
	// alone does not guarantee the trace is complete (engine_test's note).
	waitRunEnd := func(tid oteltrace.TraceID, what string) {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if _, ok := stubNamed(stubsOfTrace(exp, tid), "run.end"); ok {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("%s: no run.end span landed on trace %s", what, tid)
	}

	// assertOneTrace pins the acceptance shape on ONE run's trace: the
	// gateway → supervisor span set all share tid, the ticket key rides the
	// dispatch edges, and NO span of the trace has an unresolved parent
	// (every parent span id resolves to an exported span of this trace).
	assertOneTrace := func(tid oteltrace.TraceID, what string) {
		t.Helper()
		stubs := stubsOfTrace(exp, tid)
		// The full tree the bluebox acceptance asks for, in one trace.
		for _, name := range []string{"gateway.handle_task", "supervisor.handle_task", "run.start", "run.end", "skill.load"} {
			if _, ok := stubNamed(stubs, name); !ok {
				t.Errorf("%s: no %s span in trace %s (spans: %v)", what, name, tid, spanNames(stubs))
			}
		}
		// The ticket key rides both dispatch edges (taskspan vocabulary).
		for _, edge := range []string{"gateway.handle_task", "supervisor.handle_task"} {
			if s, ok := stubNamed(stubs, edge); ok {
				if got := stubAttr(s, "ksquad.ticket.ref"); got != "ISI-6062" {
					t.Errorf("%s: %s ksquad.ticket.ref = %q, want ISI-6062", what, edge, got)
				}
			}
		}
		// Zero unresolved parents: every span's parent (when set) resolves
		// to an exported span of this same trace — the dangling-parent
		// symptom (8d777056…/588ca000…) is a build failure here.
		ids := map[oteltrace.SpanID]bool{}
		for _, s := range stubs {
			ids[s.SpanContext.SpanID()] = true
		}
		for _, s := range stubs {
			if pid := s.Parent.SpanID(); pid.IsValid() && !ids[pid] {
				t.Errorf("%s: %s span %s parents on unexported span %s — dangling parent", what, s.Name, s.SpanContext.SpanID(), pid)
			}
		}
	}

	t1 := submitRun("task-trace-1")
	waitIdle()
	waitRunEnd(t1, "run 1")
	assertOneTrace(t1, "run 1")

	// Run #2 rides the SAME warm gateway and standing process (engineOnce:
	// the supervisor's engine survives; the stub runs again). It must root
	// its OWN trace — a pre-fix warm-pod bleed would parent run #2's spans
	// onto run #1's trace (the 0241773d… bluebox symptom).
	waitIdle()
	t2 := submitRun("task-trace-2")
	waitIdle()
	waitRunEnd(t2, "run 2")
	assertOneTrace(t2, "run 2")

	if t1 == t2 {
		t.Fatalf("run 2 rooted the SAME trace %s as run 1 — warm-pod trace bleed", t1)
	}
	// The bleed's exact shape: run #2 spans landing on run #1's trace. Every
	// supervisor edge of task 2 must live on t2, and run #1's trace must hold
	// exactly the one run it served.
	for _, s := range stubsOfTrace(exp, t1) {
		if s.Name == "supervisor.handle_task" && stubAttr(s, "ksquad.task.a2a_id") == "task-trace-2" {
			t.Errorf("run 2's supervisor.handle_task landed on run 1's trace %s — warm-pod bleed", t1)
		}
	}
	runsOn := func(tid oteltrace.TraceID) int {
		n := 0
		for _, s := range stubsOfTrace(exp, tid) {
			if s.Name == "run.start" {
				n++
			}
		}
		return n
	}
	if n := runsOn(t1); n != 1 {
		t.Errorf("run 1 trace holds %d run.start spans, want 1 — traces collapsed across runs", n)
	}
	if n := runsOn(t2); n != 1 {
		t.Errorf("run 2 trace holds %d run.start spans, want 1", n)
	}
}

// spanNames renders a stub list's span names for failure messages.
func spanNames(stubs []tracetest.SpanStub) []string {
	out := make([]string, 0, len(stubs))
	for _, s := range stubs {
		out = append(out, s.Name)
	}
	return out
}
