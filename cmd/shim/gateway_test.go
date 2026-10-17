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

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/K8squad/K8squad/pkg/a2a"
	"github.com/K8squad/K8squad/pkg/mesh"
)

// fakeGwAdapter is a scripted RuntimeAdapter for the HTTP-edge tests: each
// routed task streams one synthetic event and settles — immediately when
// autoSettle, or only when Release is called (hold mode) so a test can keep
// a process observably busy.
type fakeGwAdapter struct {
	mu         sync.Mutex
	handles    []*fakeGwHandle
	autoSettle bool
	streams    []*fakeGwStream
}

type fakeGwHandle struct {
	id string
	ep string
}

func (h *fakeGwHandle) ID() string       { return h.id }
func (h *fakeGwHandle) Endpoint() string { return h.ep }

func (f *fakeGwAdapter) Spawn(_ context.Context, env mesh.Envelope) (mesh.Handle, a2a.AgentCard, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	h := &fakeGwHandle{
		id: fmt.Sprintf("fake-%d", len(f.handles)),
		ep: fmt.Sprintf("http://127.0.0.1:%d", 9100+len(f.handles)),
	}
	f.handles = append(f.handles, h)
	return h, a2a.AgentCard{Agent: env.Identity}, nil
}

func (f *fakeGwAdapter) Register(_ context.Context, h mesh.Handle) (a2a.AgentCard, error) {
	return a2a.AgentCard{}, nil
}

func (f *fakeGwAdapter) Route(_ context.Context, h mesh.Handle, task a2a.Task) (mesh.Stream, error) {
	f.mu.Lock()
	s := &fakeGwStream{release: make(chan struct{})}
	f.streams = append(f.streams, s)
	auto := f.autoSettle
	f.mu.Unlock()
	if auto {
		s.Close()
	}
	return s, nil
}

// Release settles every stream this adapter routed that is still held.
func (f *fakeGwAdapter) Release() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.streams {
		s.Close()
	}
}

func (f *fakeGwAdapter) Hibernate(context.Context, mesh.Handle) error { return nil }
func (f *fakeGwAdapter) Wake(context.Context, mesh.Handle) error      { return nil }
func (f *fakeGwAdapter) Stop(context.Context, mesh.Handle) error      { return nil }
func (f *fakeGwAdapter) Recreate(ctx context.Context, h mesh.Handle, env mesh.Envelope) (mesh.Handle, error) {
	return h, nil
}

// fakeGwStream streams one synthetic event and settles when released —
// via Close (the Stream contract: Close unblocks Events) or auto-settle.
type fakeGwStream struct {
	release   chan struct{}
	closeOnce sync.Once
}

func (s *fakeGwStream) Events() <-chan a2a.Event {
	ch := make(chan a2a.Event, 1)
	ch <- a2a.Event{Seq: 1, Type: a2a.EventStatus}
	go func() {
		<-s.release
		close(ch)
	}()
	return ch
}

func (s *fakeGwStream) Close() error {
	s.closeOnce.Do(func() { close(s.release) })
	return nil
}

func (s *fakeGwStream) Status() a2a.Status { return a2a.Status{State: a2a.TaskCompleted} }

var _ mesh.RuntimeAdapter = (*fakeGwAdapter)(nil)

// newTestGateway builds a started gateway over the fake adapter plus the
// HTTP mux the runGateway serves, on an httptest server.
func newTestGateway(t *testing.T, min int) (*httptest.Server, *mesh.Gateway, *fakeGwAdapter) {
	t.Helper()
	fa := &fakeGwAdapter{autoSettle: true}
	gw, err := mesh.NewGateway(mesh.GatewayConfig{
		Adapter:  fa,
		Envelope: mesh.Envelope{RuntimeType: "opencode"},
		Min:      min,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := gw.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = gw.Stop(context.Background()) })

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /ready", handleGatewayReady(gw))
	mux.HandleFunc("GET /endpoints", handleGatewayEndpoints(gw))
	mux.HandleFunc("POST /task", handleGatewayTask(gw))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, gw, fa
}

// TestGatewayHandleTaskSpanCarriesTaskIdentity (ISI-6061): the
// gateway.handle_task span stamps the decoded task's identity set — the shared
// taskspan vocabulary: a2a id, work item, human ticket key, agent/team/project
// — so the mesh dispatch edge answers "whose task, which ticket" before the
// child supervisor opens its own span.
func TestGatewayHandleTaskSpanCarriesTaskIdentity(t *testing.T) {
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

	srv, _, _ := newTestGateway(t, 1)
	body := `{"a2a_task_id":"t-6061","work_item_id":"9c8b7a6d-0000-4000-8000-000000000001",` +
		`"ticket_key":"ISI-6061","identity":{"name":"coder","squad":"bravo","project":"proj-1"}}`
	resp, err := http.Post(srv.URL+"/task", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /task = %d, want 200", resp.StatusCode)
	}

	// The handler ends its span on return, which can trail the client's
	// response close — poll the exporter like the ISI-6058 sibling test
	// (TestGatewayHandleTaskContinuesTraceparentHeader) instead of asserting
	// instantly.
	deadline := time.Now().Add(2 * time.Second)
	for {
		for _, s := range exp.GetSpans() {
			if s.Name != "gateway.handle_task" {
				continue
			}
			got := map[string]string{}
			for _, kv := range s.Attributes {
				got[string(kv.Key)] = kv.Value.AsString()
			}
			for key, want := range map[string]string{
				"ksquad.task.a2a_id":       "t-6061",
				"ksquad.task.work_item_id": "9c8b7a6d-0000-4000-8000-000000000001",
				"ksquad.ticket.ref":        "ISI-6061",
				"ksquad.agent.name":        "coder",
				"ksquad.team.name":         "bravo",
				"ksquad.project.name":      "proj-1",
			} {
				if got[key] != want {
					t.Errorf("gateway.handle_task %s = %q, want %q", key, got[key], want)
				}
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no gateway.handle_task span in %d spans", len(exp.GetSpans()))
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// /ready reports the pool: ready with the process count and state buckets
// once >= 1 slot exists (a stopped slot keeps the pod Ready — recreate on
// demand is serving capability, P1.2).
func TestGatewayHTTP_Ready(t *testing.T) {
	srv, _, _ := newTestGateway(t, 2)
	resp, err := http.Get(srv.URL + "/ready")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/ready = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Serving   bool           `json:"serving"`
		Ready     bool           `json:"ready"`
		States    map[string]int `json:"states"`
		Processes int            `json:"processes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if !body.Ready || body.Processes != 2 || body.States["idle"] != 2 {
		t.Fatalf("ready body = %+v, want ready pool of 2 idle", body)
	}
}

// gatewayLifecycleFromEnv parses the D4.2 knobs; malformed values degrade to
// the default (0 → gateway default), never an error — a bad knob must not
// CrashLoop the pool pod.
func TestGatewayLifecycleFromEnv(t *testing.T) {
	t.Setenv("KSQUAD_MESH_LIFECYCLE_CHECK_INTERVAL", "45s")
	t.Setenv("KSQUAD_MESH_LIFECYCLE_IDLE_TO_HIBERNATE", "2m")
	t.Setenv("KSQUAD_MESH_LIFECYCLE_HIBERNATE_TO_STOP", "1h30m")
	t.Setenv("KSQUAD_MESH_LIFECYCLE_ALWAYS_UP", "true")

	lc := gatewayLifecycleFromEnv()
	if lc.CheckInterval != 45*time.Second {
		t.Fatalf("checkInterval = %v, want 45s", lc.CheckInterval)
	}
	if lc.IdleToHibernate != 2*time.Minute {
		t.Fatalf("idleToHibernate = %v, want 2m", lc.IdleToHibernate)
	}
	if lc.HibernateToStop != 90*time.Minute {
		t.Fatalf("hibernateToStop = %v, want 1h30m", lc.HibernateToStop)
	}
	if !lc.AlwaysUp {
		t.Fatal("alwaysUp = false, want true")
	}

	t.Setenv("KSQUAD_MESH_LIFECYCLE_CHECK_INTERVAL", "not-a-duration")
	t.Setenv("KSQUAD_MESH_LIFECYCLE_ALWAYS_UP", "")
	lc = gatewayLifecycleFromEnv()
	if lc.CheckInterval != 0 {
		t.Fatalf("malformed checkInterval = %v, want 0 (gateway default)", lc.CheckInterval)
	}
	if lc.AlwaysUp {
		t.Fatal("alwaysUp must default false")
	}

	// The S4 hibernation arm (ISI-5831): default dark; "true" arms the
	// two-tier ladder. Anything but the exact "true" stays dark — the
	// kill-switch fails closed.
	t.Setenv("KSQUAD_MESH_LIFECYCLE_HIBERNATE", "")
	if gatewayLifecycleFromEnv().Hibernate {
		t.Fatal("hibernate must default dark (collapsed P1.2 ladder)")
	}
	t.Setenv("KSQUAD_MESH_LIFECYCLE_HIBERNATE", "true")
	if !gatewayLifecycleFromEnv().Hibernate {
		t.Fatal("hibernate=true must arm the S4 two-tier ladder")
	}
	t.Setenv("KSQUAD_MESH_LIFECYCLE_HIBERNATE", "1")
	if gatewayLifecycleFromEnv().Hibernate {
		t.Fatal("only the exact \"true\" arms the ladder — fail closed")
	}
}

// /endpoints serves the live endpoint set.
func TestGatewayHTTP_Endpoints(t *testing.T) {
	srv, _, _ := newTestGateway(t, 1)
	resp, err := http.Get(srv.URL + "/endpoints")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Processes []struct {
			EndpointID string `json:"EndpointID"`
			Endpoint   string `json:"Endpoint"`
			Busy       bool   `json:"Busy"`
		} `json:"processes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Processes) != 1 || body.Processes[0].EndpointID != "fake-0" {
		t.Fatalf("endpoints body = %+v", body)
	}
}

// POST /task streams the routed task's NDJSON events; a second POST naming
// the SAME busy target gets 409 — the gateway-local one-process-one-task
// guard (D3.3 #3) surfaced on the wire.
func TestGatewayHTTP_TaskRouteAndBusy409(t *testing.T) {
	// Hold mode: the routed stream stays live until released, keeping the
	// process observably busy across requests.
	fa := &fakeGwAdapter{autoSettle: false}
	gw, err := mesh.NewGateway(mesh.GatewayConfig{
		Adapter:  fa,
		Envelope: mesh.Envelope{RuntimeType: "opencode"},
		Min:      1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := gw.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = gw.Stop(context.Background()) })
	mux := http.NewServeMux()
	mux.HandleFunc("GET /endpoints", handleGatewayEndpoints(gw))
	mux.HandleFunc("POST /task", handleGatewayTask(gw))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// A task whose stream we hold open: the handler stays in its event
	// loop, the process stays busy.
	first, err := http.Post(srv.URL+"/task", "application/json", strings.NewReader(`{"a2a_task_id":"t-1"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer first.Body.Close()

	// The busy flag flips as soon as the route is in flight.
	deadline := time.Now().Add(2 * time.Second)
	ready := false
	for time.Now().Before(deadline) {
		resp, err := http.Get(srv.URL + "/endpoints")
		if err != nil {
			t.Fatal(err)
		}
		var body struct {
			Processes []struct {
				Busy bool `json:"Busy"`
			} `json:"processes"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		if len(body.Processes) == 1 && body.Processes[0].Busy {
			ready = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !ready {
		t.Fatal("process never flipped busy while the routed stream was live")
	}

	second, err := http.Post(srv.URL+"/task?target=fake-0", "application/json",
		strings.NewReader(`{"a2a_task_id":"t-2"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer second.Body.Close()
	if second.StatusCode != http.StatusConflict {
		t.Fatalf("busy target POST = %d, want 409", second.StatusCode)
	}

	// Settle the held stream: the process releases and routes again. The
	// release is asynchronous — the handler must observe the stream end
	// before the gateway flips the process idle — so poll for the flag
	// instead of racing the POST against it (CI legs run loaded; the
	// immediate POST flaked as 409 under contention).
	fa.Release()
	deadline = time.Now().Add(2 * time.Second)
	released := false
	for time.Now().Before(deadline) {
		resp, err := http.Get(srv.URL + "/endpoints")
		if err != nil {
			t.Fatal(err)
		}
		var body struct {
			Processes []struct {
				Busy bool `json:"Busy"`
			} `json:"processes"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		if len(body.Processes) == 1 && !body.Processes[0].Busy {
			released = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !released {
		t.Fatal("process never released after the held stream settled")
	}
	third, err := http.Post(srv.URL+"/task", "application/json",
		strings.NewReader(`{"a2a_task_id":"t-3"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer third.Body.Close()
	if third.StatusCode != http.StatusOK {
		t.Fatalf("POST after settle = %d, want 200", third.StatusCode)
	}
}

// A task whose stream already settled routes again cleanly — the busy flag
// releases at stream settle (no stuck-idle-pool-of-one regression).
func TestGatewayHTTP_TaskSettlesAndReRoutes(t *testing.T) {
	srv, _, _ := newTestGateway(t, 1)
	for i := 0; i < 3; i++ {
		resp, err := http.Post(srv.URL+"/task", "application/json",
			strings.NewReader(fmt.Sprintf(`{"a2a_task_id":"t-%d"}`, i)))
		if err != nil {
			t.Fatal(err)
		}
		// Drain the whole NDJSON body so the stream settles before the next lap.
		var sb strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := resp.Body.Read(buf)
			sb.Write(buf[:n])
			if err != nil {
				break
			}
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("lap %d POST = %d, want 200", i, resp.StatusCode)
		}
		if !strings.Contains(sb.String(), `"seq"`) {
			t.Fatalf("lap %d body missing streamed event: %q", i, sb.String())
		}
	}
}

// buildGateway is fail-closed on the pool identity: no team/role, no gateway.
func TestBuildGatewayRequiresPoolIdentity(t *testing.T) {
	t.Setenv("KSQUAD_MESH_TEAM", "")
	t.Setenv("KSQUAD_MESH_ROLE", "")
	if _, err := buildGateway(); err == nil {
		t.Fatal("expected pool identity validation error")
	}
}

// The composition-root runtime binding (P2 close-out follow-up, ISI-5826
// comment b09e1970 — additive, outside the pkg/mesh seam): the declared
// runtime type binds the matching RuntimeAdapter; an untyped pod keeps the
// opencode default; an unknown type fails closed (never silently run
// opencode under another runtime's image).
func TestBuildRuntimeAdapterBindsDeclaredType(t *testing.T) {
	host := &mesh.LocalHost{}
	for _, tc := range []struct {
		runtimeType string
		wantErr     bool
	}{
		{"opencode", false}, // adapter #1 — the pre-seam default
		{"codex", false},    // adapter #2 — the P2 proof runtime
		{"", true},          // never reached via buildGateway (env default)
		{"martian", true},   // fail closed
	} {
		adapter, err := buildRuntimeAdapter(tc.runtimeType, host)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("runtime type %q must fail closed", tc.runtimeType)
			}
			continue
		}
		if err != nil {
			t.Fatalf("runtime type %q: %v", tc.runtimeType, err)
		}
		if adapter == nil {
			t.Fatalf("runtime type %q bound no adapter", tc.runtimeType)
		}
	}
}

// buildGateway assembles through the same binding: a codex pool builds
// (the runtime re-check — a second runtime drives the gateway with zero
// control-plane change), and an unknown type fails the build honestly.
func TestBuildGatewayBindsRuntimeTypeFromEnv(t *testing.T) {
	t.Setenv("KSQUAD_MESH_TEAM", "team-acme")
	t.Setenv("KSQUAD_MESH_ROLE", "coder")
	t.Setenv("KSQUAD_RUNTIME_TYPE", "codex")
	if _, err := buildGateway(); err != nil {
		t.Fatalf("codex pool must build: %v", err)
	}
	t.Setenv("KSQUAD_RUNTIME_TYPE", "martian")
	if _, err := buildGateway(); err == nil {
		t.Fatal("unknown runtime type must fail the build closed")
	}
	t.Setenv("KSQUAD_RUNTIME_TYPE", "")
	if _, err := buildGateway(); err != nil {
		t.Fatalf("untyped pod must keep the opencode default: %v", err)
	}
}

// TestGatewayHandleTaskContinuesTraceparentHeader (ISI-6058, ISI-6055 P0):
// the dispatch POST's W3C carrier (traceparent header) makes
// gateway.handle_task CONTINUE the upstream trace — trace and parent span id
// equal the carrier's — instead of rooting a fresh one. Without a carrier the
// span roots exactly as before (a pre-seam caller keeps its behavior).
func TestGatewayHandleTaskContinuesTraceparentHeader(t *testing.T) {
	prevTP := otel.GetTracerProvider()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		otel.SetTracerProvider(prevTP)
	})

	srv, _, _ := newTestGateway(t, 1)

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/task", strings.NewReader(`{"a2a_task_id":"t-1"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("traceparent", "00-eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee-ffffffffffffffff-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("gateway POST = %d, want 200", resp.StatusCode)
	}

	// The handler ends its span on return, which can trail the client's
	// response close — poll the exporter instead of asserting instantly.
	waitForSpan := func(filter func(tracetest.SpanStub) bool) tracetest.SpanStub {
		deadline := time.Now().Add(2 * time.Second)
		for {
			for _, s := range exp.GetSpans() {
				if s.Name == "gateway.handle_task" && filter(s) {
					return s
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("gateway.handle_task span never appeared, got %d spans", len(exp.GetSpans()))
			}
			time.Sleep(2 * time.Millisecond)
		}
	}

	handleSpan := waitForSpan(func(s tracetest.SpanStub) bool {
		return s.SpanContext.TraceID().String() == "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	})
	if handleSpan.Parent.SpanID().String() != "ffffffffffffffff" {
		t.Errorf("gateway.handle_task parent = %s, want the carrier span ffff…", handleSpan.Parent.SpanID())
	}

	// No carrier on the wire: the span roots a fresh trace (pre-seam shape).
	resp2, err := http.Post(srv.URL+"/task", "application/json", strings.NewReader(`{"a2a_task_id":"t-2"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	rootSpan := waitForSpan(func(s tracetest.SpanStub) bool {
		return s.SpanContext.TraceID().String() != "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	})
	if rootSpan.Parent.IsValid() {
		t.Errorf("gateway.handle_task parent = %s without a carrier, want a fresh root", rootSpan.Parent.SpanID())
	}
}

// ISI-6123: the standing supervisor's extra env always carries the mesh
// run-path label, and — when the pool grants skills via KSQUAD_MESH_SKILLS —
// the granted ref LIST as KSQUAD_SKILLS, so the supervisor's engine reads a
// non-empty cfg.Skills and fires skill.load spans. Envelope.Skills alone only
// ever reached the agent card, which left mesh traces with zero skill.load
// spans (the ISI-6062 finding).
func TestGatewayStandingEnvStampsSkills(t *testing.T) {
	// Granted skills ride KSQUAD_SKILLS as a CSV next to the run-path label.
	env := gatewayStandingEnv([]string{"paperclip", "code-review"})
	if got := env["KSQUAD_RUN_PATH"]; got != "mesh" {
		t.Fatalf("KSQUAD_RUN_PATH = %q, want mesh", got)
	}
	if got := env["KSQUAD_SKILLS"]; got != "paperclip,code-review" {
		t.Fatalf("KSQUAD_SKILLS = %q, want paperclip,code-review", got)
	}

	// Bare pool (no skills): the label stamps, KSQUAD_SKILLS does not — so a
	// bare mesh process keeps an empty cfg.Skills rather than a stray "".
	bare := gatewayStandingEnv(nil)
	if got := bare["KSQUAD_RUN_PATH"]; got != "mesh" {
		t.Fatalf("bare KSQUAD_RUN_PATH = %q, want mesh", got)
	}
	if _, ok := bare["KSQUAD_SKILLS"]; ok {
		t.Fatal("bare pool must not stamp KSQUAD_SKILLS")
	}
}
