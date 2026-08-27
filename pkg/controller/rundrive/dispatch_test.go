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

package rundrive

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/expfmt"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	_ "github.com/jackc/pgx/v5/stdlib" // lazy *sql.DB handles for config validation

	api "github.com/K8squad/K8squad/api/v1alpha1"
	wire "github.com/K8squad/K8squad/pkg/a2a"
	"github.com/K8squad/K8squad/pkg/telemetry/toolusage"
)

func dispatchScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := api.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

// lazyDB returns a *sql.DB handle that never connects in these tests (pgx
// connects lazily); the coord source seam is faked where queries matter.
func lazyDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", "host=localhost port=1 dbname=unused connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

type fakeDispatchSource struct {
	title string
	body  string
	fence string
}

func (f fakeDispatchSource) WorkItem(context.Context, string) (string, string, error) {
	return f.title, f.body, nil
}

func (f fakeDispatchSource) FenceToken(context.Context, string) (string, error) {
	return f.fence, nil
}

// TestHelperProcess is not a real test: exec'd as the fake `shim run`
// subprocess (the StdioTransport contract), it mimics cmd/shim.driveRun —
// one Task JSON on stdin, JSONL SSE on stdout — plus a tool event pair so
// the telemetry mapping has something to map, and a dump of the received
// task for the assertions.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	var task wire.Task
	if err := json.NewDecoder(os.Stdin).Decode(&task); err != nil {
		fmt.Fprintln(os.Stderr, "helper: decode task:", err)
		os.Exit(2)
	}
	if dump := os.Getenv("KSQUAD_TASK_DUMP"); dump != "" {
		_ = os.WriteFile(dump, mustJSON(task), 0o600)
	}
	if env := os.Getenv("KSQUAD_HELPER_ENV_DUMP"); env != "" {
		_ = os.WriteFile(env, []byte(strings.Join(os.Args, "\x00")), 0o600)
	}
	ok := true
	events := []wire.Event{
		{Seq: 1, A2ATaskID: task.A2ATaskID, Type: wire.EventStatus, Payload: wire.StatusPayload{State: wire.TaskWorking}},
		{Seq: 2, A2ATaskID: task.A2ATaskID, Type: wire.EventTool, Payload: wire.ToolPayload{
			Name: "shell", Phase: "start", Skill: "git", ArgsSHA256: "deadbeef",
		}},
		{Seq: 3, A2ATaskID: task.A2ATaskID, Type: wire.EventTool, Payload: wire.ToolPayload{
			Name: "shell", Phase: "result", Skill: "git", OK: &ok,
		}},
		{Seq: 4, A2ATaskID: task.A2ATaskID, Type: wire.EventStatus, Payload: wire.StatusPayload{State: wire.TaskCompleted}},
	}
	enc := json.NewEncoder(os.Stdout)
	for _, ev := range events {
		if err := enc.Encode(ev); err != nil {
			os.Exit(2)
		}
	}
	os.Exit(0)
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// TestOperatorDispatcherProducesScrapeableSeries is the ISI-3352 acceptance
// at the wiring level — the operator-shaped physical feed chain, every
// component real: dispatch identifiers → Dispatcher → StdioTransport
// spawning an actual `shim run` subprocess → TelemetrySink → the
// operator-registered toolusage mapper → a real Prometheus registry. A
// driven Run must yield a scrapeable ksquad_tool_calls_total series carrying
// the Run's agent label (what the D3 read model + panel consume off
// /metrics).
func TestOperatorDispatcherProducesScrapeableSeries(t *testing.T) {
	const runUID = "11111111-2222-3333-4444-555555555555"
	run := &api.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: "team-a", UID: types.UID(runUID)},
		Spec: api.RunSpec{
			TeamRef:     api.ObjectRef{Name: "team-a"},
			ProjectRef:  api.ObjectRef{Name: "proj-1"},
			WorkItemRef: "99999999-8888-7777-6666-555555555555",
			Agents:      []api.ObjectRef{{Name: "coder"}},
		},
	}
	agent := &api.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "coder", Namespace: "team-a"},
		Spec:       api.AgentSpec{Model: "claude-sonnet-4"},
	}
	cl := fake.NewClientBuilder().WithScheme(dispatchScheme(t)).WithObjects(run, agent).Build()

	reg := prometheus.NewRegistry()
	mapper := toolusage.NewMapper(nil, reg) // nil tracer: metrics-only (spans noop)

	var forwarded []wire.Event
	inner := captureSink{evs: &forwarded}

	dump := t.TempDir() + "/task.json"
	d, err := NewOperatorDispatcher(OperatorDispatchConfig{
		DB:          lazyDB(t),
		Client:      cl,
		Mapper:      mapper,
		ShimBin:     os.Args[0], // the test binary re-execs as the fake shim
		RuntimeType: "opencode",
		RunEvents:   inner,
		ExtraEnv:    []string{"GO_WANT_HELPER_PROCESS=1", "KSQUAD_TASK_DUMP=" + dump},
		Source:      fakeDispatchSource{title: "Fix the flake", body: "make test/flake green", fence: "7"},
	})
	if err != nil {
		t.Fatalf("NewOperatorDispatcher: %v", err)
	}
	if err := d.Submit(context.Background(), runUID, runUID); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	d.Wait() // background follow settles before assertions

	// The task the shim received carries the coord + Run CR seams.
	b, err := os.ReadFile(dump)
	if err != nil {
		t.Fatalf("read task dump: %v", err)
	}
	var got wire.Task
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal task: %v", err)
	}
	if got.A2ATaskID != runUID {
		t.Errorf("task.a2a_task_id = %q, want run uid", got.A2ATaskID)
	}
	if got.WorkItemID != run.Spec.WorkItemRef {
		t.Errorf("task.work_item_id = %q, want %q", got.WorkItemID, run.Spec.WorkItemRef)
	}
	if got.FenceToken != "7" {
		t.Errorf("task.fence_token = %q, want 7", got.FenceToken)
	}
	if got.Envelope.Input != "make test/flake green" {
		t.Errorf("task.envelope.input = %q, want the work item body", got.Envelope.Input)
	}
	if got.Envelope.Metadata["team"] != "team-a" || got.Envelope.Metadata["project"] != "proj-1" {
		t.Errorf("task.envelope.metadata = %v, want team/project refs", got.Envelope.Metadata)
	}
	if got.CredentialsMounted {
		t.Error("task.credentials_ref_mounted must be false in the v1 operator-spawned topology")
	}

	// Every event was forwarded verbatim to the inner run-events sink.
	if len(forwarded) != 4 {
		t.Fatalf("inner sink saw %d events, want 4", len(forwarded))
	}

	// Scrape the registry exactly like the D3 read model scrapes /metrics.
	exposition := gatherExposition(t, reg)
	if !strings.Contains(exposition, "ksquad_tool_usage_pipeline_up") {
		t.Error("exposition lacks the pipeline-liveness marker")
	}
	if !strings.Contains(exposition, `ksquad_tool_calls_total{agent="coder",skill="git",tool="shell"}`) {
		t.Errorf("exposition lacks ksquad_tool_calls_total{agent=\"coder\"} series:\n%s", exposition)
	}
}

type captureSink struct{ evs *[]wire.Event }

func (c captureSink) Event(_ context.Context, ev wire.Event) error {
	*c.evs = append(*c.evs, ev)
	return nil
}

func gatherExposition(t *testing.T, reg *prometheus.Registry) string {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var sb strings.Builder
	enc := expfmt.NewEncoder(&sb, expfmt.NewFormat(expfmt.TypeTextPlain))
	for _, mf := range mfs {
		if err := enc.Encode(mf); err != nil {
			t.Fatalf("encode: %v", err)
		}
	}
	return sb.String()
}

func TestNewOperatorDispatcherValidation(t *testing.T) {
	cl := fake.NewClientBuilder().WithScheme(dispatchScheme(t)).Build()
	if _, err := NewOperatorDispatcher(OperatorDispatchConfig{Client: cl}); err == nil {
		t.Fatal("nil DB must error")
	}
	if _, err := NewOperatorDispatcher(OperatorDispatchConfig{DB: lazyDB(t)}); err == nil {
		t.Fatal("nil Client must error")
	}
	// A missing shim binary is the ledger-only signal: an error, not a panic.
	if _, err := NewOperatorDispatcher(OperatorDispatchConfig{
		DB: lazyDB(t), Client: cl, ShimBin: "/nonexistent/ksquad-shim",
	}); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing shim err = %v, want LookPath failure", err)
	}
}

// TestCleanRunID pins the lap/disambiguation-suffix strip (boundTaskID
// shapes: runID, runID#lapN, runID/fixture).
func TestCleanRunID(t *testing.T) {
	for in, want := range map[string]string{
		"run-1":       "run-1",
		"run-1#lap2":  "run-1",
		"run-1/run-1": "run-1",
		"run-1#lap2x": "run-1",
	} {
		if got := cleanRunID(in); got != want {
			t.Errorf("cleanRunID(%q) = %q, want %q", in, got, want)
		}
	}
}
