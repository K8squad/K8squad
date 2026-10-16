/*
Copyright 2026 The K8sAgentMesh Authors.

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

package mesh

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/K8sAgentMesh/K8sAgentMesh/a2a"
)

// meshChildHelperEnv arms the re-exec'd standing-process stand-in the P2
// routed-run proof spawns (see TestMain).
const meshChildHelperEnv = "MESH_TEST_CHILD_HELPER"

// testCodexType is the runtime flavor name the module tests inject for codex
// (a host injects its own constant; k8squad host: api/v1alpha1
// RuntimeTypeCodex).
const testCodexType = "codex"

// testCodexSpec is the injected runtime profile standing in for a host's
// declarative codex runtime — plain values, exactly what a host plugin
// supplies at wiring time (k8squad host: pkg/shim/runtimes +
// internal/protocol + its SchemaVersion stamp).
func testCodexSpec() RuntimeSpec {
	return RuntimeSpec{
		Type:           testCodexType,
		CLIVersion:     "rust-v0.152.0",
		Capabilities:   a2a.Capabilities{Streaming: true, ToolCalls: true, InteractivePrompt: true, BYOModelEndpoint: true, ArtifactKinds: []string{"file", "patch"}, Docker: true, GitHub: true, PackageInstall: true},
		DefaultModel:   a2a.ModelInfo{ID: "gpt-5.4-codex", ContextWindow: 272000},
		CredentialType: "apikey",
		SchemaVersion:  a2a.DefaultSchemaVersion,
		Protocol:       a2a.ProtocolVersions{A2A: "0.2.0", MCP: "2025-06-18"},
		RuntimeTypeEnv: testRuntimeTypeEnv,
	}
}

// TestMain is the package's re-exec trampoline for the P2 routed-run proof:
// when the test binary is launched WITH the child-helper env set it IS the
// standing process — it never runs the test suite.
func TestMain(m *testing.M) {
	if os.Getenv(meshChildHelperEnv) == "1" {
		meshChildHelper()
		return
	}
	os.Exit(m.Run())
}

// meshChildHelper is the standing-process stand-in the REAL LocalHost
// substrate execs in TestCodexP2_RoutedRunThroughGateway. It honors the launch
// contract the codex adapter built: it binds the KSQUAD_SUPERVISOR_ADDR
// LocalHost assigns it and serves the shim supervisor's POST /task NDJSON
// contract. The fake is drawn exactly where the chapter draws the seams —
// everything above the substrate (gateway lifecycle, adapter, router, the A2A
// wire decode) is production code; only the runtime CLI behind the supervisor
// engine is scripted, because the real `codex` binary and its credential are
// not present under test.
func meshChildHelper() {
	addr := os.Getenv(envSupervisorAddr)
	if addr == "" {
		fmt.Fprintln(os.Stderr, "mesh child helper: KSQUAD_SUPERVISOR_ADDR unset")
		os.Exit(64)
	}
	runtimeType := os.Getenv(testRuntimeTypeEnv)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("POST /task", func(w http.ResponseWriter, r *http.Request) {
		var task a2a.Task
		if err := json.NewDecoder(r.Body).Decode(&task); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// The honest flavor check: the launch env the codex adapter built for
		// this standing process must tell the shim supervisor to serve codex.
		// A 500 here fails the routed-run proof loudly at the route step.
		if runtimeType != testCodexType {
			http.Error(w, "standing process flavor = "+runtimeType+", want "+testCodexType, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		enc := json.NewEncoder(w)
		_ = enc.Encode(a2a.Event{
			Seq:       1,
			A2ATaskID: task.A2ATaskID,
			TS:        time.Now(),
			Type:      a2a.EventMessage,
			Payload:   a2a.MessagePayload{Role: "assistant", Text: "codex standing process at work", Trust: "untrusted"},
		})
		_ = enc.Encode(a2a.Event{
			Seq:       2,
			A2ATaskID: task.A2ATaskID,
			TS:        time.Now(),
			Type:      a2a.EventStatus,
			Payload: a2a.StatusPayload{
				State:  a2a.TaskCompleted,
				Reason: "codex child served " + task.A2ATaskID,
			},
		})
	})
	if err := http.ListenAndServe(addr, mux); err != nil {
		fmt.Fprintf(os.Stderr, "mesh child helper: serve %s: %v\n", addr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func TestNewCodexAdapterRequiresHost(t *testing.T) {
	if _, err := NewCodexAdapter(nil, testCodexSpec()); err == nil {
		t.Fatal("NewCodexAdapter must reject a nil host")
	}
}

// The injected-spec seam fails loudly at wiring time (the injected analogue of
// the host registry's "runtime not registered"), never mid-Spawn.
func TestNewCodexAdapterRequiresValidSpec(t *testing.T) {
	spec := testCodexSpec()
	spec.Type = ""
	if _, err := NewCodexAdapter(&fakeHost{}, spec); err == nil {
		t.Fatal("NewCodexAdapter must reject an empty runtime type")
	}
	spec = testCodexSpec()
	spec.RuntimeTypeEnv = ""
	if _, err := NewCodexAdapter(&fakeHost{}, spec); err == nil {
		t.Fatal("NewCodexAdapter must reject an empty launch env contract")
	}
}

func TestCodexSpawnStampsRuntimeAndCard(t *testing.T) {
	host := &fakeHost{}
	spec := testCodexSpec()
	a, err := NewCodexAdapter(host, spec)
	if err != nil {
		t.Fatalf("NewCodexAdapter: %v", err)
	}

	_, card, err := a.Spawn(context.Background(), Envelope{
		Skills:   []string{"code-review"},
		Identity: a2a.AgentIdentity{Name: "coder", Squad: "eng", Project: "k8squad"},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	// The launched process is told to be codex via the injected env contract —
	// exactly how the standing process's supervisor picks its baked-in flavor.
	if len(host.specs) != 1 {
		t.Fatalf("expected one launch, got %d", len(host.specs))
	}
	if host.specs[0].RuntimeType != spec.Type {
		t.Fatalf("launch runtime type = %q, want %q", host.specs[0].RuntimeType, spec.Type)
	}
	if !hasEnv(host.specs[0].Env, spec.RuntimeTypeEnv+"="+spec.Type) {
		t.Fatalf("launch env %v missing %s=%s", host.specs[0].Env, spec.RuntimeTypeEnv, spec.Type)
	}

	// The card must mirror the injected runtime spec's honest contract — the
	// spec is the one source of truth, so the mesh never advertises what the
	// process cannot do.
	if card.Runtime.Type != spec.Type {
		t.Fatalf("card runtime type = %q, want %q", card.Runtime.Type, spec.Type)
	}
	if card.Runtime.CLIVersion != spec.CLIVersion {
		t.Fatalf("card CLI version = %q, want %q", card.Runtime.CLIVersion, spec.CLIVersion)
	}
	if card.Model.ID != spec.DefaultModel.ID {
		t.Fatalf("card model = %q, want default %q", card.Model.ID, spec.DefaultModel.ID)
	}
	if !reflect.DeepEqual(card.Capabilities, spec.Capabilities) {
		t.Fatalf("card capabilities = %+v, want %+v", card.Capabilities, spec.Capabilities)
	}
	if card.Auth.Type != spec.CredentialType {
		t.Fatalf("card auth shape = %q, want %q", card.Auth.Type, spec.CredentialType)
	}
	if card.Agent.Name != "coder" || card.Agent.Project != "k8squad" {
		t.Fatalf("card identity = %+v, want coder/k8squad", card.Agent)
	}
	if card.SchemaVersion != spec.SchemaVersion {
		t.Fatalf("card schema = %q, want %q", card.SchemaVersion, spec.SchemaVersion)
	}
	if card.Protocol != spec.Protocol {
		t.Fatalf("card protocol = %+v, want %+v", card.Protocol, spec.Protocol)
	}
}

func TestCodexModelOverride(t *testing.T) {
	host := &fakeHost{}
	spec := testCodexSpec()
	a, _ := NewCodexAdapter(host, spec)
	_, card, err := a.Spawn(context.Background(), Envelope{Model: "gpt-5.4-high"})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if card.Model.ID != "gpt-5.4-high" {
		t.Fatalf("card model = %q, want the override gpt-5.4-high", card.Model.ID)
	}
	// The context window stays the runtime's authority, never the override's.
	if card.Model.ContextWindow != spec.DefaultModel.ContextWindow {
		t.Fatalf("context window = %d, want runtime default %d", card.Model.ContextWindow, spec.DefaultModel.ContextWindow)
	}
}

func TestCodexBYOModelRouteEnv(t *testing.T) {
	host := &fakeHost{}
	a, _ := NewCodexAdapter(host, testCodexSpec())
	_, _, err := a.Spawn(context.Background(), Envelope{
		ModelRoute: a2a.ModelRoute{Endpoint: "http://ollama:11434/v1", Model: "qwen3"},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	env := host.specs[0].Env
	if !hasEnv(env, "OPENAI_BASE_URL=http://ollama:11434/v1") {
		t.Fatalf("env %v missing OPENAI_BASE_URL", env)
	}
	// The zero-credential Ollama lane gets the conventional placeholder key —
	// the same env the codex runtime's per-task Command maps the route onto.
	if !hasEnv(env, "OPENAI_API_KEY=ollama") {
		t.Fatalf("env %v missing the ollama placeholder key", env)
	}
}

func TestCodexEnvOverrideWins(t *testing.T) {
	host := &fakeHost{}
	a, _ := NewCodexAdapter(host, testCodexSpec())
	_, _, err := a.Spawn(context.Background(), Envelope{
		ModelRoute: a2a.ModelRoute{Endpoint: "http://e/v1"},
		Env:        map[string]string{"OPENAI_API_KEY": "sk-real"},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	env := host.specs[0].Env
	if !hasEnv(env, "OPENAI_API_KEY=sk-real") {
		t.Fatalf("caller env must override the placeholder; got %v", env)
	}
	if hasEnv(env, "OPENAI_API_KEY=ollama") {
		t.Fatalf("placeholder key must not survive a caller override; got %v", env)
	}
}

func TestCodexLaunchEnvDeterministic(t *testing.T) {
	host := &fakeHost{}
	a, _ := NewCodexAdapter(host, testCodexSpec())
	env := Envelope{Env: map[string]string{"B": "2", "A": "1"}}
	if _, _, err := a.Spawn(context.Background(), env); err != nil {
		t.Fatalf("Spawn 1: %v", err)
	}
	if _, _, err := a.Spawn(context.Background(), env); err != nil {
		t.Fatalf("Spawn 2: %v", err)
	}
	if strings.Join(host.specs[0].Env, ",") != strings.Join(host.specs[1].Env, ",") {
		t.Fatalf("the same envelope must yield a byte-identical spec: %v vs %v", host.specs[0].Env, host.specs[1].Env)
	}
}

// The codex adapter must be usable purely through the RuntimeAdapter seam —
// the gateway never sees a concrete type (the "zero control-plane change at
// P2" test). The constructor succeeding against the same PlatformHost/Option
// surface adapter #1 uses is the assertion.
func TestCodexIsRuntimeAdapter(t *testing.T) {
	if _, err := NewCodexAdapter(&fakeHost{}, testCodexSpec()); err != nil {
		t.Fatalf("NewCodexAdapter: %v", err)
	}
}

// TestCodexP2_RoutedRunThroughGateway is the P2 acceptance proof (chapter
// §9.3, ISI-5826): a REAL run routed through adapter #2 with ZERO
// control-plane change. Every mesh-side component in the path is production
// code — the mesh.Gateway lifecycle (spawn → idle → route → settle), the codex
// ChildProcessAdapter (launch env + card from the injected codex runtime
// spec), the LocalHost substrate (real exec of a real OS process in its own
// process group) and the default HTTP /task router (real POST, real NDJSON
// tail). Only the runtime CLI behind the child's /task surface is scripted.
//
// The composition this test compiles and runs IS the seam proof: the gateway
// is constructed with Adapter: NewCodexAdapter(host, spec) and manages the
// pool purely through the 7 RuntimeAdapter ops — nothing in gateway/router/
// lifecycle/supervisor knows a second runtime exists.
func TestCodexP2_RoutedRunThroughGateway(t *testing.T) {
	// Reserve a loopback port for the standing process's A2A listener, then
	// hand it to LocalHost (the production host derives base+i; a single
	// fixed slot keeps the test hermetic).
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := lis.Addr().String()
	_ = lis.Close()

	// Arm the re-exec child BEFORE Start so the process LocalHost execs
	// serves the supervisor's /task contract on the assigned addr.
	t.Setenv(meshChildHelperEnv, "1")

	host := &LocalHost{
		BinPath: os.Args[0],             // the test binary re-execs as the child
		Args:    []string{"supervisor"}, // the production subcommand shape
		Addr:    func(int) string { return addr },
		Stdout:  io.Discard,
		Stderr:  io.Discard,
	}
	adapter, err := NewCodexAdapter(host, testCodexSpec())
	if err != nil {
		t.Fatalf("NewCodexAdapter: %v", err)
	}

	ctx := context.Background()
	gw, err := NewGateway(GatewayConfig{
		Adapter: adapter,
		Envelope: Envelope{
			RuntimeType: testCodexType,
			Identity:    a2a.AgentIdentity{Name: "coder", Squad: "team-acme", Project: "k8squad"},
		},
		Min:  1,
		Team: "team-acme",
		Role: "coder",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := gw.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = gw.Stop(ctx) })

	// Wait for the spawned standing process to serve: the pool's spawn is
	// async to the child's listen, so readiness is polled on the child's
	// own surface (the same contract the pod probes).
	healthURL := "http://" + addr + "/health"
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, herr := http.Get(healthURL)
		if herr == nil {
			_ = resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("codex standing process did not come up at %s: %v", addr, herr)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Route ONE run through the gateway — empty target, the idle tier — and
	// tail its live event stream to the settle.
	const taskID = "cccccccc-0000-4444-8888-cccccccccccc"
	stream, err := gw.Route(ctx, "", a2a.Task{
		A2ATaskID: taskID,
		Identity:  a2a.AgentIdentity{Squad: "team-acme", Name: "coder"},
	})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	events := 0
	for ev := range stream.Events() {
		events++
		if ev.Type != a2a.EventMessage && ev.Type != a2a.EventStatus {
			t.Fatalf("unexpected event type %q on the routed stream", ev.Type)
		}
		if ev.A2ATaskID != taskID {
			t.Fatalf("event %d carries task %q, want %q", ev.Seq, ev.A2ATaskID, taskID)
		}
	}
	if events < 2 {
		t.Fatalf("routed run streamed %d events, want the message + terminal status sequence", events)
	}
	st := stream.Status()
	if st.State != a2a.TaskCompleted {
		t.Fatalf("routed run settled %q, want completed (reason %q)", st.State, st.Reason)
	}
	if !strings.Contains(st.Reason, taskID) {
		t.Fatalf("settled reason %q does not round-trip the task id %s — the run did not reach the codex child", st.Reason, taskID)
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}

	// The run settled and the slot must be back to IDLE for the next route —
	// the gateway's lifecycle view of a codex process is identical to
	// opencode's (the seam contract, not the runtime flavor).
	procs := gw.Processes()
	if len(procs) != 1 || procs[0].State != StateIdle {
		t.Fatalf("post-settle pool = %+v, want one IDLE codex process", procs)
	}
}
