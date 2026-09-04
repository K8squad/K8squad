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
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	api "github.com/K8squad/K8squad/api/v1alpha1"
	wire "github.com/K8squad/K8squad/pkg/a2a"
	"github.com/K8squad/K8squad/pkg/taskio"
)

const (
	taskIORunUID = "11111111-2222-3333-4444-555555555555"
	taskIOWorkID = "99999999-8888-7777-6666-555555555555"
)

func taskIOTestKey() []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = byte(i + 7)
	}
	return k
}

// dispatchForTaskIO builds an operatorDispatch over a fake client carrying one
// Run + Agent, with the shim env env-only knobs set from opts.
func dispatchForTaskIO(t *testing.T, minter *taskio.Minter, coordURL string) *operatorDispatch {
	t.Helper()
	run := &api.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: "team-a", UID: types.UID(taskIORunUID)},
		Spec: api.RunSpec{
			TeamRef:     api.ObjectRef{Name: "team-a"},
			ProjectRef:  api.ObjectRef{Name: "proj-1"},
			WorkItemRef: taskIOWorkID,
			Agents:      []api.ObjectRef{{Name: "coder"}},
		},
	}
	agent := &api.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "coder", Namespace: "team-a"},
		Spec:       api.AgentSpec{Model: "claude-sonnet-4"},
	}
	cl := fake.NewClientBuilder().WithScheme(dispatchScheme(t)).WithObjects(run, agent).Build()
	return &operatorDispatch{
		cfg: OperatorDispatchConfig{
			DB:             lazyDB(t),
			Client:         cl,
			RuntimeType:    "opencode",
			TaskIOMinter:   minter,
			TaskIOCoordURL: coordURL,
		},
		shimBin: "shim",
	}
}

func envMap(t *testing.T, env []string) map[string]string {
	t.Helper()
	m := make(map[string]string, len(env))
	for _, kv := range env {
		if i := strings.IndexByte(kv, '='); i >= 0 {
			m[kv[:i]] = kv[i+1:]
		}
	}
	return m
}

// AC6: the four run-scoped bootstrap vars are present in the curated shim env,
// the token is a valid run-scoped token bound to (RUN_ID, WORK_ITEM_ID), and no
// operator secret (DATABASE_URL) leaks into the subprocess.
func TestShimEnvInjectsTaskIOVars(t *testing.T) {
	minter, err := taskio.NewMinter(taskIOTestKey(), time.Hour)
	if err != nil {
		t.Fatalf("NewMinter: %v", err)
	}
	const coordURL = "http://ksquad-apiserver.ksquad-system.svc:8080/api/task-io"
	d := dispatchForTaskIO(t, minter, coordURL)

	cmd, err := d.shimCommand(context.Background(), wire.Task{A2ATaskID: taskIORunUID})
	if err != nil {
		t.Fatalf("shimCommand: %v", err)
	}
	env := envMap(t, cmd.Env)

	if env[taskio.EnvCoordURL] != coordURL {
		t.Errorf("%s = %q, want %q", taskio.EnvCoordURL, env[taskio.EnvCoordURL], coordURL)
	}
	if env[taskio.EnvWorkItemID] != taskIOWorkID {
		t.Errorf("%s = %q, want %q", taskio.EnvWorkItemID, env[taskio.EnvWorkItemID], taskIOWorkID)
	}
	if env[taskio.EnvRunID] != taskIORunUID {
		t.Errorf("%s = %q, want %q", taskio.EnvRunID, env[taskio.EnvRunID], taskIORunUID)
	}
	tok := env[taskio.EnvCoordToken]
	if tok == "" {
		t.Fatalf("%s missing", taskio.EnvCoordToken)
	}
	rt, err := minter.Verify(tok)
	if err != nil {
		t.Fatalf("injected token does not verify: %v", err)
	}
	if rt.RunID != taskIORunUID || rt.WorkItemID != taskIOWorkID {
		t.Errorf("token binding = %+v, want run/work %s/%s", rt, taskIORunUID, taskIOWorkID)
	}
	if rt.Principal != "coder" {
		t.Errorf("token principal = %q, want the agent name %q", rt.Principal, "coder")
	}

	// Minimal-env invariant: no operator secret reaches the subprocess.
	if _, leaked := env["DATABASE_URL"]; leaked {
		t.Error("DATABASE_URL leaked into the shim env (minimal-env invariant broken)")
	}
}

// With no minter/URL configured, injection is skipped wholesale — fail-safe,
// and the curated env is otherwise unchanged.
func TestShimEnvSkipsTaskIOWhenUnconfigured(t *testing.T) {
	d := dispatchForTaskIO(t, nil, "")
	cmd, err := d.shimCommand(context.Background(), wire.Task{A2ATaskID: taskIORunUID})
	if err != nil {
		t.Fatalf("shimCommand: %v", err)
	}
	env := envMap(t, cmd.Env)
	for _, k := range []string{taskio.EnvCoordURL, taskio.EnvCoordToken, taskio.EnvWorkItemID, taskio.EnvRunID} {
		if _, ok := env[k]; ok {
			t.Errorf("%s present but task-io is unconfigured", k)
		}
	}
	// The existing curated vars still land.
	if env["KSQUAD_SQUAD"] != "team-a" {
		t.Errorf("KSQUAD_SQUAD = %q, want team-a", env["KSQUAD_SQUAD"])
	}
}
