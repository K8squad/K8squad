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

// Command shim is the KSquad agent-runtime sidecar (arch §7.5, stories 5.5 +
// 5.8): one binary, one image per AgentRuntime.type, selected at launch from
// the registered v1 shim set (openclaw, hermes, opencode). It is the target
// Dockerfile.shim builds.
//
// Subcommands:
//
//	shim card         Emit the generated Agent Card (spec §6.1) as JSON and exit.
//	                  Used by the reconciler and the conformance suite (5.6).
//	shim run          Read one A2A Task (spec §3 V1) as JSON from stdin, drive it
//	                  through the runtime, stream sequenced SSE events (spec §4)
//	                  as JSONL to stdout, and exit non-zero on a failed task.
//	shim read         Answer a read-only build-browser query (tree | diff | file |
//	                  meta) against the Run's live worktree and print it as JSON with
//	                  live:true (story 8.7b). Runtime-agnostic; no engine needed.
//
// The runtime flavor and Agent config are read from the environment the
// reconciler injects (arch §7.2/§7.3); the raw credential is held only in
// memory and is never logged.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/K8squad/K8squad/pkg/a2a"
	"github.com/K8squad/K8squad/pkg/shim"
	"github.com/K8squad/K8squad/pkg/shim/runtimes"
)

// version is stamped at build time (-ldflags "-X main.version=...").
var version = "dev"

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "shim:", err)
		os.Exit(1)
	}
}

func run(args []string, stdin io.Reader, stdout io.Writer) error {
	cmd := "card"
	if len(args) > 0 {
		cmd = args[0]
	}

	// The 8.7b read verb is runtime-agnostic: it reads the Run's workspace, so it needs no runtime
	// selection or engine. Handle it before requiring KSQUAD_RUNTIME_TYPE.
	if cmd == "read" {
		return driveRead(args[1:], stdout)
	}

	runtimeType := env("KSQUAD_RUNTIME_TYPE", os.Getenv("RUNTIME"))
	if runtimeType == "" {
		return fmt.Errorf("no runtime selected: set KSQUAD_RUNTIME_TYPE to one of %v", runtimes.Registered())
	}
	rt, err := runtimes.Get(runtimeType)
	if err != nil {
		return err
	}

	cfg := configFromEnv()
	engine := shim.New(rt, shim.NewOSRunner(), cfg)

	switch cmd {
	case "card":
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(engine.AgentCard())
	case "run":
		return driveRun(engine, stdin, stdout)
	default:
		return fmt.Errorf("unknown subcommand %q (want: card | run | read)", cmd)
	}
}

// driveRun reads one Task from stdin, submits it, streams its SSE events as
// JSONL to stdout until terminal, and returns an error on a non-success task
// so the container exit code reflects the Run outcome.
func driveRun(engine *shim.Engine, stdin io.Reader, stdout io.Writer) error {
	var task a2a.Task
	if err := json.NewDecoder(stdin).Decode(&task); err != nil {
		return fmt.Errorf("decode task: %w", err)
	}

	ctx := context.Background()
	if _, err := engine.SubmitTask(ctx, task); err != nil {
		return err
	}

	events, err := engine.StreamEvents(ctx, task.A2ATaskID, 0)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(stdout)
	for ev := range events {
		if err := enc.Encode(ev); err != nil {
			return err
		}
	}

	status, err := engine.GetStatus(ctx, task.A2ATaskID)
	if err != nil {
		return err
	}
	if status.State != a2a.TaskCompleted {
		return fmt.Errorf("run %s ended %s: %s", task.A2ATaskID, status.State, status.Reason)
	}
	return nil
}

func configFromEnv() shim.Config {
	return shim.Config{
		Identity: shim.Identity{
			Name:    os.Getenv("KSQUAD_AGENT_NAME"),
			Squad:   os.Getenv("KSQUAD_SQUAD"),
			Project: os.Getenv("KSQUAD_PROJECT"),
		},
		Skills:              splitList(os.Getenv("KSQUAD_SKILLS")),
		Model:               os.Getenv("KSQUAD_MODEL"),
		Credential:          os.Getenv("KSQUAD_CREDENTIAL"),
		CredentialSecretRef: os.Getenv("KSQUAD_CREDENTIAL_SECRET_REF"),
		ShimVersion:         version,
		Experimental:        os.Getenv("KSQUAD_EXPERIMENTAL") == "true",
		WorkDir:             os.Getenv("KSQUAD_WORKDIR"),
	}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func splitList(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
