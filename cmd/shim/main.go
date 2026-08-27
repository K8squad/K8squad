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

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/expfmt"

	"github.com/K8squad/K8squad/pkg/a2a"
	"github.com/K8squad/K8squad/pkg/capability"
	"github.com/K8squad/K8squad/pkg/shim"
	"github.com/K8squad/K8squad/pkg/shim/runtimes"
	"github.com/K8squad/K8squad/pkg/telemetry"
	"github.com/K8squad/K8squad/pkg/telemetry/toolusage"
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

	cfg, err := configFromEnv()
	if err != nil {
		return err
	}

	// Epic D (plan §2.4): the tool-usage telemetry spine. The toggle env is
	// rendered by the operator from the OTelConfig CRD tool-usage pipeline
	// (D2); default ON. The trace carrier (traceparent/tracestate) joins
	// this shim's spans onto the Run's distributed trace when the
	// reconciler propagated one (warmpool Boot stamps it into the sandbox
	// env via telemetry.Inject).
	toolusage.SetEnabled(env("KSQUAD_TOOL_USAGE_ENABLED", "true") != "false")
	ctx := telemetry.Extract(context.Background(), traceCarrierFromEnv())
	// Telemetry writes to STDERR, never stdout: `shim run`'s stdout IS the
	// SSE JSONL wire (spec §4) — span/log records on stdout would corrupt
	// the event stream (ISI-3348 review).
	_, otelShutdown, terr := telemetry.Setup(ctx, telemetry.Options{ServiceName: "ksquad-shim", Writer: os.Stderr})
	if terr == nil {
		defer func() { _ = otelShutdown(context.Background()) }()
	}

	// The mapper rides a REAL registry (ISI-3348 finding 1): the shim is a
	// one-shot process (spawn-per-task), so pull-model scraping does not fit
	// it — instead the final exposition is exported at process exit to the
	// textfile path (KSQUAD_PROM_TEXTFILE, the node-exporter textfile
	// convention) for whatever surface collects it in the deployment
	// topology. Unset (default) skips the write; spans still flow via the
	// telemetry spine.
	metricsReg := prometheus.NewRegistry()
	mapper := toolusage.NewMapper(telemetry.Tracer(), metricsReg)

	engine := shim.New(rt, shim.NewOSRunner(), cfg)
	engine.SetTelemetry(mapper)

	switch cmd {
	case "card":
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(engine.AgentCard())
	case "run":
		err := driveRun(engine, stdin, stdout)
		writeMetricsTextfile(metricsReg)
		return err
	default:
		return fmt.Errorf("unknown subcommand %q (want: card | run | read)", cmd)
	}
}

// writeMetricsTextfile exports the shim's ksquad_* exposition (D2) to the
// KSQUAD_PROM_TEXTFILE path when set. Best-effort and never fatal: telemetry
// is observational; a failed export must not flip the task's exit code.
func writeMetricsTextfile(reg *prometheus.Registry) {
	path := os.Getenv("KSQUAD_PROM_TEXTFILE")
	if path == "" {
		return
	}
	mfs, err := reg.Gather()
	if err != nil {
		fmt.Fprintf(os.Stderr, "shim: gather metrics: %v\n", err)
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "shim: open metrics textfile: %v\n", err)
		return
	}
	defer func() { _ = f.Close() }()
	enc := expfmt.NewEncoder(f, expfmt.NewFormat(expfmt.TypeTextPlain))
	for _, mf := range mfs {
		if err := enc.Encode(mf); err != nil {
			fmt.Fprintf(os.Stderr, "shim: encode metrics: %v\n", err)
			return
		}
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

func configFromEnv() (shim.Config, error) {
	cfg := shim.Config{
		Identity: shim.Identity{
			Name:    os.Getenv("KSQUAD_AGENT_NAME"),
			Squad:   os.Getenv("KSQUAD_SQUAD"),
			Project: os.Getenv("KSQUAD_PROJECT"),
		},
		Skills:              splitList(os.Getenv("KSQUAD_SKILLS")),
		SkillSHAs:           parseSkillSHAs(os.Getenv("KSQUAD_SKILL_SHAS")),
		Model:               os.Getenv("KSQUAD_MODEL"),
		Credential:          os.Getenv("KSQUAD_CREDENTIAL"),
		CredentialSecretRef: os.Getenv("KSQUAD_CREDENTIAL_SECRET_REF"),
		ShimVersion:         version,
		Experimental:        os.Getenv("KSQUAD_EXPERIMENTAL") == "true",
		WorkDir:             os.Getenv("KSQUAD_WORKDIR"),
	}
	// Epic C (ADR-044 step 6): the projected MCP IR, parsed once at
	// startup — fail-closed on a set-but-broken document so a Run never
	// serves with a silently missing capability envelope.
	endpoints, err := capability.LoadMCPConfig(os.Getenv(capability.MCPConfigEnvVar))
	if err != nil {
		return shim.Config{}, err
	}
	cfg.MCPEndpoints = endpoints
	return cfg, nil
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// traceCarrierFromEnv lifts W3C trace-context headers out of the process env
// so the shim's spans continue the trace the reconciler started (the spine's
// Inject wrote them when rendering the sandbox env — warmpool Boot). The
// carrier keys are lowercase per propagation.MapCarrier/W3C; the env vars
// follow the UPPERCASE TRACEPARENT/TRACESTATE convention.
func traceCarrierFromEnv() map[string]string {
	carrier := map[string]string{}
	for envName, carrierKey := range map[string]string{"TRACEPARENT": "traceparent", "TRACESTATE": "tracestate"} {
		if v := os.Getenv(envName); v != "" {
			carrier[carrierKey] = v
		}
	}
	return carrier
}

// parseSkillSHAs parses "name=sha256,name=sha256" — the reconciler-rendered
// pin map for git-sourced skills (Epic D: the SHA rides skill.load spans).
// Malformed entries are skipped, never fatal: telemetry is observational.
func parseSkillSHAs(s string) map[string]string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	m := map[string]string{}
	for _, part := range strings.Split(s, ",") {
		name, sha, found := strings.Cut(strings.TrimSpace(part), "=")
		if found && name != "" && sha != "" {
			m[name] = sha
		}
	}
	if len(m) == 0 {
		return nil
	}
	return m
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
