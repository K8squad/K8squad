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

package a2a_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"

	clienta2a "github.com/K8squad/K8squad/internal/a2a"
	wire "github.com/K8squad/K8squad/pkg/a2a"
)

// TestHelperProcess is not a real test: exec'd as the fake shim `run` subprocess
// by TestStdioTransportDrivesSubprocessShim, it mimics cmd/shim.driveRun —
// reads one Task JSON from stdin, streams a JSONL SSE sequence to stdout, and
// exits with a code reflecting the terminal state.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	var task wire.Task
	if err := json.NewDecoder(os.Stdin).Decode(&task); err != nil {
		fmt.Fprintln(os.Stderr, "helper: decode task:", err)
		os.Exit(2)
	}
	enc := json.NewEncoder(os.Stdout)
	events := []wire.Event{
		{Seq: 1, A2ATaskID: task.A2ATaskID, Type: wire.EventStatus, Payload: wire.StatusPayload{State: wire.TaskWorking}},
		{Seq: 2, A2ATaskID: task.A2ATaskID, Type: wire.EventArtifactRef, Payload: wire.ArtifactRef{Kind: "patch", WorkItemID: task.WorkItemID, URI: "file:///out.patch", SHA256: "cafef00d"}},
		{Seq: 3, A2ATaskID: task.A2ATaskID, Type: wire.EventStatus, Payload: wire.StatusPayload{State: wire.TaskCompleted}},
	}
	for _, ev := range events {
		if err := enc.Encode(ev); err != nil {
			os.Exit(2)
		}
	}
	os.Exit(0)
}

func TestStdioTransportDrivesSubprocessShim(t *testing.T) {
	tr := &clienta2a.StdioTransport{
		Command: func(ctx context.Context, _ wire.Task) (*exec.Cmd, error) {
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestHelperProcess")
			cmd.Env = append(os.Environ(), "GO_WANT_HELPER_PROCESS=1")
			return cmd, nil
		},
	}
	client := clienta2a.New(tr)
	sink := &collectSink{}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res, err := client.Dispatch(ctx, wire.Task{A2ATaskID: "run-1", WorkItemID: "wi-1"}, sink)
	if err != nil {
		t.Fatalf("Dispatch over stdio: %v", err)
	}
	if len(sink.events) != 3 {
		t.Fatalf("sink saw %d events, want 3: %+v", len(sink.events), sink.events)
	}
	if res.Status.State != wire.TaskCompleted {
		t.Fatalf("terminal state = %q, want completed", res.Status.State)
	}
	if len(res.Artifacts) != 1 || res.Artifacts[0].URI != "file:///out.patch" || res.Artifacts[0].SHA256 != "cafef00d" {
		t.Fatalf("artifacts = %+v, want the one JSONL-decoded ref", res.Artifacts)
	}
	if res.LastSeq != 3 {
		t.Fatalf("LastSeq = %d, want 3", res.LastSeq)
	}
}
