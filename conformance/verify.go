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

package conformance

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/K8squad/K8squad/pkg/a2a"
	"github.com/K8squad/K8squad/pkg/shim"
	"github.com/K8squad/K8squad/pkg/shim/runtimes"
)

const (
	// sentinelCredential is the raw secret the harness feeds the shim so the
	// credential-metadata check can prove it never reaches the Agent Card or
	// the SSE wire (NFR-SEC3). It is deliberately unmistakable.
	sentinelCredential = "SENTINEL-CONFORMANCE-CREDENTIAL-do-not-leak"
	// sentinelSecretRef is the Secret NAME advertised on the card auth block —
	// metadata the shim is allowed to surface (never the value above).
	sentinelSecretRef = "conformance-agent-credential" // #nosec G101 -- a Secret NAME (metadata), not a credential value.
	// ollamaPlaceholderKey is the conventional zero-cost key the OpenAI-client
	// wire uses against a local Ollama endpoint (see runtimes.modelRouteEnv).
	ollamaPlaceholderKey = "ollama"

	conformanceRunID  = "run-conformance-0001"
	conformanceWorkID = "work-conformance-0001"
)

// Options configures a conformance run.
type Options struct {
	// Lane selects the model-provider lane (LaneDefault or LaneOllama).
	Lane Lane
	// OllamaEndpoint is the BYO OpenAI-compatible endpoint used on LaneOllama.
	// Defaults to a conventional local Ollama URL; the assertions are wire-shape
	// assertions and do not require the endpoint to be reachable, so the lane
	// runs at $0 with no live server.
	OllamaEndpoint string
	// OllamaModel is the model id resolved at the Ollama endpoint on LaneOllama.
	OllamaModel string
}

func (o Options) ollamaEndpoint() string {
	if o.OllamaEndpoint != "" {
		return o.OllamaEndpoint
	}
	return "http://ollama:11434/v1"
}

func (o Options) ollamaModel() string {
	if o.OllamaModel != "" {
		return o.OllamaModel
	}
	return "qwen3"
}

// VerifyRuntime runs the full conformance suite against a registered runtime
// adapter and returns a Report. It is the vendor entrypoint: register your
// runtimes.Runtime, call VerifyRuntime, and a passing Report means "works in
// any squad, zero core changes." VerifyRuntime never returns an error — every
// failure is a Result on the Report so a caller (test or CLI) gets the full
// picture in one pass.
func VerifyRuntime(rt runtimes.Runtime, opts Options) Report {
	c := newChecker()
	h := &harness{rt: rt, opts: opts}

	// LaneOllama eligibility: a runtime that does not advertise byoModelEndpoint
	// cannot be certified on the Ollama lane (story 5.7 — the capability gates
	// the model-provider seam). This is itself a capability-honesty assertion.
	if opts.Lane == LaneOllama && !rt.Capabilities().BYOModelEndpoint {
		c.fail(CheckCapabilityHonesty,
			"runtime %q does not advertise byoModelEndpoint; not eligible for the Ollama lane", rt.Type())
		// Still validate the static card so the report is complete.
		h.checkAgentCard(c, h.newEngine(nil).AgentCard())
		return c.report(rt.Type(), opts.Lane)
	}

	h.run(c)
	return c.report(rt.Type(), opts.Lane)
}

// harness drives one runtime through the engine and records check results.
type harness struct {
	rt   runtimes.Runtime
	opts Options
}

// newEngine builds a shim.Engine for the runtime backed by runner, with the
// per-lane launch config (credential + model on the default lane; zero paid
// credential on the Ollama lane).
func (h *harness) newEngine(runner shim.Runner) *shim.Engine {
	cfg := shim.Config{
		Identity:            shim.Identity{Name: "conformance-agent", Squad: "conformance-squad", Project: "conformance"},
		Skills:              []string{"conformance"},
		CredentialSecretRef: sentinelSecretRef,
		ShimVersion:         "conformance",
		WorkDir:             "/tmp/conformance",
		Nower:               func() time.Time { return time.Unix(0, 0).UTC() },
	}
	if h.opts.Lane != LaneOllama {
		// Default lane rides the runtime's fixed vendor wire with a real
		// (sentinel) provider credential the leak scan hunts for.
		cfg.Credential = sentinelCredential
	}
	// Ollama lane leaves cfg.Credential empty: the $0 property is that no paid
	// provider key is present — the model wire carries only the placeholder key.
	return shim.New(h.rt, runner, cfg)
}

// newTask builds the conformance Task for the current lane.
func (h *harness) newTask() a2a.Task {
	t := a2a.Task{
		A2ATaskID:          conformanceRunID,
		WorkItemID:         conformanceWorkID,
		FenceToken:         "fence-1",
		Envelope:           a2a.Envelope{SystemContext: "you are under conformance test", Input: "emit progress and one artifact per advertised kind"},
		CredentialsMounted: h.opts.Lane != LaneOllama,
	}
	if h.opts.Lane == LaneOllama {
		// BYO Ollama endpoint, zero-cost placeholder token (story 5.7/5.8).
		t.ModelRoute = a2a.ModelRoute{Endpoint: h.opts.ollamaEndpoint(), Model: h.opts.ollamaModel()}
	}
	return t
}

// run executes the full assertion set for one lane.
func (h *harness) run(c *checker) {
	card := h.rt.Capabilities()
	artifactKinds := card.ArtifactKinds

	runner := &scriptedRunner{
		steps:   conformantPlan(conformanceWorkID, artifactKinds),
		outcome: shim.Outcome{State: a2a.TaskCompleted},
	}
	engine := h.newEngine(runner)
	agentCard := engine.AgentCard()

	// (1) Agent Card validity.
	h.checkAgentCard(c, agentCard)

	// Drive one full task to terminal, collecting the SSE log.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := engine.SubmitTask(ctx, h.newTask()); err != nil {
		c.fail(CheckTaskLifecycle, "SubmitTask failed: %v", err)
		return
	}
	events, err := engine.StreamEvents(ctx, conformanceRunID, 0)
	if err != nil {
		c.fail(CheckSSEProgress, "StreamEvents failed: %v", err)
		return
	}
	var log []a2a.Event
	for ev := range events {
		log = append(log, ev)
	}

	// (2) Task lifecycle, (3) SSE progress, (4) artifact emission.
	h.checkLifecycle(c, ctx, engine, runner, log)
	h.checkSSE(c, ctx, engine, log)
	h.checkArtifacts(c, agentCard, log)

	// (5) Capability honesty — the run must not have exercised any capability
	// the card did not advertise, and the streaming/artifact-kind claims held.
	h.checkCapabilityHonesty(c, agentCard, log, runner)

	// (6) Credential-metadata correctness.
	h.checkCredentialMetadata(c, agentCard, log, runner)
}

// checkAgentCard asserts the Agent Card schema is valid and complete (spec §6.1).
func (h *harness) checkAgentCard(c *checker, card a2a.AgentCard) {
	var problems []string
	if card.SchemaVersion != a2a.SchemaVersion {
		problems = append(problems, fmt.Sprintf("schemaVersion=%q want %q", card.SchemaVersion, a2a.SchemaVersion))
	}
	if card.Runtime.Type == "" {
		problems = append(problems, "runtime.type empty")
	}
	if _, err := runtimes.Get(card.Runtime.Type); err != nil {
		problems = append(problems, fmt.Sprintf("runtime.type %q not registered", card.Runtime.Type))
	}
	if card.Runtime.CLIVersion == "" {
		problems = append(problems, "runtime.cliVersion empty (ADR-017 reproducibility)")
	}
	if card.Model.ID == "" {
		problems = append(problems, "model.id empty")
	}
	if card.Model.ContextWindow <= 0 {
		problems = append(problems, "model.contextWindow must be > 0 (budget authority §6.2)")
	}
	// SSE (V2) is mandatory: a shim that cannot stream cannot be driven.
	if !card.Capabilities.Streaming {
		problems = append(problems, "capabilities.streaming must be true (SSE V2 is mandatory)")
	}
	if len(card.Capabilities.ArtifactKinds) == 0 {
		problems = append(problems, "capabilities.artifactKinds empty (a conformant runtime emits ≥1 kind, §5)")
	}
	if card.Protocol.A2A == "" || card.Protocol.MCP == "" {
		problems = append(problems, "protocol pins must advertise explicit A2A + MCP revisions (story 5.3)")
	}
	c.require(CheckAgentCard, len(problems) == 0,
		fmt.Sprintf("schema %s, runtime %s/%s, model %s (ctx %d), protocol a2a=%s mcp=%s",
			card.SchemaVersion, card.Runtime.Type, card.Runtime.CLIVersion, card.Model.ID,
			card.Model.ContextWindow, card.Protocol.A2A, card.Protocol.MCP),
		strings.Join(problems, "; "))
}

// checkLifecycle asserts the §3.1 state machine, submit-reattach dedup (C1) and
// idempotent cancel (C8).
func (h *harness) checkLifecycle(c *checker, ctx context.Context, engine *shim.Engine, runner *scriptedRunner, log []a2a.Event) {
	var problems []string

	// Terminal state is completed for the conformant plan.
	st, err := engine.GetStatus(ctx, conformanceRunID)
	if err != nil {
		problems = append(problems, fmt.Sprintf("GetStatus after run: %v", err))
	} else if st.State != a2a.TaskCompleted {
		problems = append(problems, fmt.Sprintf("terminal state=%s want completed", st.State))
	} else if !st.State.IsTerminal() {
		problems = append(problems, "terminal state not reported terminal")
	}

	// First status event is submitted; a working state is reached before terminal.
	if seenStates := statusStates(log); len(seenStates) == 0 || seenStates[0] != a2a.TaskSubmitted {
		problems = append(problems, fmt.Sprintf("first status event=%v want submitted first", seenStates))
	} else if !containsState(seenStates, a2a.TaskWorking) {
		problems = append(problems, "never transitioned through working")
	} else if last := seenStates[len(seenStates)-1]; !last.IsTerminal() {
		problems = append(problems, fmt.Sprintf("last status event=%s not terminal", last))
	}

	// C1: a re-submit with the same id reattaches — no second launch.
	before := runner.launchCount()
	reStatus, err := engine.SubmitTask(ctx, h.newTask())
	if err != nil {
		problems = append(problems, fmt.Sprintf("re-SubmitTask failed: %v", err))
	} else if reStatus.State != a2a.TaskCompleted {
		problems = append(problems, fmt.Sprintf("re-submit returned %s want the terminal completed (dedup C1)", reStatus.State))
	}
	if after := runner.launchCount(); after != before {
		problems = append(problems, fmt.Sprintf("re-submit launched the runtime again (%d→%d), violating dedup C1", before, after))
	}

	// C8: cancel on a terminal task is an idempotent no-op success.
	if err := engine.CancelTask(ctx, conformanceRunID, "post-terminal cancel"); err != nil {
		problems = append(problems, fmt.Sprintf("CancelTask on terminal task errored: %v (must be no-op, C8)", err))
	}
	// C8: cancel on an unknown task is an idempotent no-op success.
	if err := engine.CancelTask(ctx, "task-never-seen", "unknown"); err != nil {
		problems = append(problems, fmt.Sprintf("CancelTask on unknown task errored: %v (must be no-op, C8)", err))
	}

	// Mid-flight cancel drains to canceled (fresh engine + blocking runtime).
	if detail := h.checkCancelDrain(); detail != "" {
		problems = append(problems, detail)
	}

	c.require(CheckTaskLifecycle, len(problems) == 0,
		"submitted→working→completed; reattach dedup (C1) and idempotent cancel (C8) hold",
		strings.Join(problems, "; "))
}

// checkCancelDrain verifies a live task cancels to the terminal canceled state
// and that a second cancel is a no-op (C8). Returns "" on success.
func (h *harness) checkCancelDrain() string {
	runner := &scriptedRunner{
		steps:            conformantPlan(conformanceWorkID, h.rt.Capabilities().ArtifactKinds),
		blockUntilCancel: true,
	}
	engine := h.newEngine(runner)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := engine.SubmitTask(ctx, h.newTask()); err != nil {
		return fmt.Sprintf("cancel-drain SubmitTask failed: %v", err)
	}
	// Let the runtime reach working before cancelling.
	if err := waitForState(ctx, engine, conformanceRunID, a2a.TaskWorking); err != nil {
		return fmt.Sprintf("cancel-drain never reached working: %v", err)
	}
	if err := engine.CancelTask(ctx, conformanceRunID, "conformance cancel"); err != nil {
		return fmt.Sprintf("CancelTask on live task errored: %v", err)
	}
	st, err := engine.GetStatus(ctx, conformanceRunID)
	if err != nil {
		return fmt.Sprintf("GetStatus after cancel: %v", err)
	}
	if st.State != a2a.TaskCanceled {
		return fmt.Sprintf("cancelled task state=%s want canceled", st.State)
	}
	if err := engine.CancelTask(ctx, conformanceRunID, "again"); err != nil {
		return fmt.Sprintf("second CancelTask errored: %v (must be no-op, C8)", err)
	}
	return ""
}

// checkSSE asserts gap-free monotonic sequencing and gap-free resume (C4).
func (h *harness) checkSSE(c *checker, ctx context.Context, engine *shim.Engine, log []a2a.Event) {
	var problems []string
	if len(log) == 0 {
		c.fail(CheckSSEProgress, "no SSE events produced")
		return
	}
	// Monotonic, gap-free, starting at seq 1.
	for i, ev := range log {
		want := uint64(i + 1)
		if ev.Seq != want {
			problems = append(problems, fmt.Sprintf("event %d has seq %d want %d (not gap-free monotonic, C4)", i, ev.Seq, want))
			break
		}
		if ev.A2ATaskID != conformanceRunID {
			problems = append(problems, fmt.Sprintf("event %d task id=%q want %q", i, ev.A2ATaskID, conformanceRunID))
			break
		}
	}
	if log[0].Type != a2a.EventStatus {
		problems = append(problems, "first event is not a status event")
	}
	if last := log[len(log)-1]; last.Type != a2a.EventStatus {
		problems = append(problems, "last event is not a terminal status event")
	}

	// Resume from a mid-stream seq returns only seq>fromSeq, still gap-free.
	if len(log) >= 3 {
		from := log[1].Seq
		resumed, err := engine.StreamEvents(ctx, conformanceRunID, from)
		if err != nil {
			problems = append(problems, fmt.Sprintf("resume StreamEvents failed: %v", err))
		} else {
			var got []a2a.Event
			for ev := range resumed {
				got = append(got, ev)
			}
			if len(got) != len(log)-2 {
				problems = append(problems, fmt.Sprintf("resume from seq %d returned %d events want %d", from, len(got), len(log)-2))
			}
			for i, ev := range got {
				if ev.Seq != from+uint64(i)+1 {
					problems = append(problems, fmt.Sprintf("resume gap: event %d seq %d want %d", i, ev.Seq, from+uint64(i)+1))
					break
				}
			}
		}
	}

	c.require(CheckSSEProgress, len(problems) == 0,
		fmt.Sprintf("%d events, gap-free monotonic seq 1..%d, resume gap-free (C4)", len(log), log[len(log)-1].Seq),
		strings.Join(problems, "; "))
}

// checkArtifacts asserts §5 artifact-ref shape and work-item binding.
func (h *harness) checkArtifacts(c *checker, card a2a.AgentCard, log []a2a.Event) {
	refs := artifactRefs(log)
	if len(refs) == 0 {
		c.fail(CheckArtifactEmission, "no artifact-ref events emitted (a conformant run produces ≥1, §5)")
		return
	}
	var problems []string
	for i, ref := range refs {
		if ref.WorkItemID != conformanceWorkID {
			problems = append(problems, fmt.Sprintf("artifact %d work_item_id=%q want %q (must bind to the Run's item, §5)", i, ref.WorkItemID, conformanceWorkID))
		}
		if ref.URI == "" {
			problems = append(problems, fmt.Sprintf("artifact %d has empty uri", i))
		}
		if len(ref.SHA256) != 64 {
			problems = append(problems, fmt.Sprintf("artifact %d sha256 not a 64-hex content hash (content-addressed, §5)", i))
		}
		if ref.Kind == "" {
			problems = append(problems, fmt.Sprintf("artifact %d has empty kind", i))
		}
	}
	c.require(CheckArtifactEmission, len(problems) == 0,
		fmt.Sprintf("%d artifact-ref(s), all content-addressed and bound to work_item %s", len(refs), conformanceWorkID),
		strings.Join(problems, "; "))
}

// checkCapabilityHonesty asserts the run exercised no capability the card did
// not advertise (F15): every emitted artifact kind is on the card, no
// interactive-prompt state was reached unless advertised, and tool events only
// appear when toolCalls is advertised.
func (h *harness) checkCapabilityHonesty(c *checker, card a2a.AgentCard, log []a2a.Event, runner *scriptedRunner) {
	var problems []string
	caps := card.Capabilities

	// Every emitted artifact kind must be advertised.
	emittedKinds := uniqueKinds(artifactRefs(log))
	if missing, ok := isSubset(emittedKinds, caps.ArtifactKinds); !ok {
		problems = append(problems, fmt.Sprintf("emitted unadvertised artifact kind(s): %s (advertised: %v)", missing, caps.ArtifactKinds))
	}

	// Tool events only when toolCalls is advertised.
	if hasEventType(log, a2a.EventTool) && !caps.ToolCalls {
		problems = append(problems, "emitted tool events but capabilities.toolCalls=false")
	}

	// input-required is only reachable when interactivePrompt is advertised (C6).
	if containsState(statusStates(log), a2a.TaskInputRequired) && !caps.InteractivePrompt {
		problems = append(problems, "reached input-required without capabilities.interactivePrompt (C6)")
	}

	// The runtime was actually launched (the card is not a paper claim).
	if runner.launchCount() == 0 {
		problems = append(problems, "runtime never launched — capability claims unexercised")
	}

	c.require(CheckCapabilityHonesty, len(problems) == 0,
		fmt.Sprintf("kinds %v ⊇ emitted %v; toolCalls=%v honored; no undisclosed input-required",
			caps.ArtifactKinds, emittedKinds, caps.ToolCalls),
		strings.Join(problems, "; "))
}

// checkCredentialMetadata asserts the auth block is metadata only and the raw
// credential never reaches the card or the SSE wire (NFR-SEC3), plus — on the
// Ollama lane — that the model wire carries only the zero-cost placeholder key
// and no paid provider credential (the $0 property, story 5.7/5.8).
func (h *harness) checkCredentialMetadata(c *checker, card a2a.AgentCard, log []a2a.Event, runner *scriptedRunner) {
	var problems []string

	// Auth block shape + metadata-only.
	shape := card.Auth.Type
	if shape != string(runtimes.ShapeAPIKey) && shape != string(runtimes.ShapeOAuthToken) {
		problems = append(problems, fmt.Sprintf("auth.type=%q is not a known credential shape", shape))
	}
	if card.Auth.SecretRef == "" {
		problems = append(problems, "auth.secretRef empty (the card must name the Secret so the reconciler can mount it)")
	}
	if card.Auth.SecretRef == sentinelCredential {
		problems = append(problems, "auth.secretRef carries the raw credential value, not a reference")
	}

	// The raw credential must not appear on the card.
	if leaked, err := containsSecret(card, sentinelCredential); err != nil {
		problems = append(problems, err.Error())
	} else if leaked {
		problems = append(problems, "raw credential leaked into the Agent Card")
	}
	// ...nor on any SSE event (F16/NFR-SEC3).
	if leaked, err := containsSecret(log, sentinelCredential); err != nil {
		problems = append(problems, err.Error())
	} else if leaked {
		problems = append(problems, "raw credential leaked into an SSE event payload")
	}

	// Ollama lane: prove the $0 model wire.
	if h.opts.Lane == LaneOllama {
		spec, ok := runner.lastSpec()
		if !ok {
			problems = append(problems, "no ExecSpec captured for the Ollama lane")
		} else {
			env := envMap(spec.Env)
			if env["OPENAI_BASE_URL"] != h.opts.ollamaEndpoint() {
				problems = append(problems, fmt.Sprintf("OPENAI_BASE_URL=%q want the BYO Ollama endpoint %q", env["OPENAI_BASE_URL"], h.opts.ollamaEndpoint()))
			}
			if env["OPENAI_API_KEY"] != ollamaPlaceholderKey {
				problems = append(problems, fmt.Sprintf("OPENAI_API_KEY=%q want the zero-cost placeholder %q (no paid credential)", env["OPENAI_API_KEY"], ollamaPlaceholderKey))
			}
			// No env var may carry the sentinel paid credential on this lane.
			for k, v := range env {
				if v == sentinelCredential {
					problems = append(problems, fmt.Sprintf("paid credential present in %s on the $0 Ollama lane", k))
				}
			}
		}
	}

	detail := "auth.type=" + shape + " secretRef=" + card.Auth.SecretRef + "; raw credential absent from card + wire"
	if h.opts.Lane == LaneOllama {
		detail = "Ollama lane: model wire uses placeholder key, zero paid credential; " + detail
	}
	c.require(CheckCredentialMetadata, len(problems) == 0, detail, strings.Join(problems, "; "))
}

// waitForState polls the shim until the task reaches want or ctx expires.
func waitForState(ctx context.Context, engine *shim.Engine, taskID string, want a2a.TaskState) error {
	for {
		st, err := engine.GetStatus(ctx, taskID)
		if err != nil {
			return err
		}
		if st.State == want || st.State.IsTerminal() {
			if st.State == want {
				return nil
			}
			return fmt.Errorf("reached terminal %s before %s", st.State, want)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
}

// --- small pure helpers over the SSE log ---

func statusStates(log []a2a.Event) []a2a.TaskState {
	var out []a2a.TaskState
	for _, ev := range log {
		if ev.Type != a2a.EventStatus {
			continue
		}
		if sp, ok := ev.Payload.(a2a.StatusPayload); ok {
			out = append(out, sp.State)
		}
	}
	return out
}

func artifactRefs(log []a2a.Event) []a2a.ArtifactRef {
	var out []a2a.ArtifactRef
	for _, ev := range log {
		if ev.Type != a2a.EventArtifactRef {
			continue
		}
		if ar, ok := ev.Payload.(a2a.ArtifactRef); ok {
			out = append(out, ar)
		}
	}
	return out
}

func uniqueKinds(refs []a2a.ArtifactRef) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, r := range refs {
		if _, ok := seen[r.Kind]; ok {
			continue
		}
		seen[r.Kind] = struct{}{}
		out = append(out, r.Kind)
	}
	return out
}

func hasEventType(log []a2a.Event, t a2a.EventType) bool {
	for _, ev := range log {
		if ev.Type == t {
			return true
		}
	}
	return false
}

func containsState(states []a2a.TaskState, want a2a.TaskState) bool {
	for _, s := range states {
		if s == want {
			return true
		}
	}
	return false
}

func envMap(env []string) map[string]string {
	m := make(map[string]string, len(env))
	for _, kv := range env {
		if i := strings.IndexByte(kv, '='); i >= 0 {
			m[kv[:i]] = kv[i+1:]
		}
	}
	return m
}
