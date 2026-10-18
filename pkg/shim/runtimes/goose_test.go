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

package runtimes

import (
	"encoding/json"
	"strings"
	"testing"

	yaml "sigs.k8s.io/yaml"

	"github.com/K8squad/K8squad/pkg/a2a"
	"github.com/K8squad/K8squad/pkg/capability"
)

// gooseLC builds a LaunchContext for the goose command tests.
func gooseLC() LaunchContext {
	return LaunchContext{
		Envelope: a2a.Envelope{SystemContext: "sys ctx", Input: "do the thing"},
		Model:    "qwen3.5:35b",
		WorkDir:  "/sandbox/wd",
	}
}

// envMap flattens an ExecSpec env slice for assertions.
func envMap(env []string) map[string]string {
	m := map[string]string{}
	for _, e := range env {
		parts := strings.SplitN(e, "=", 2)
		m[parts[0]] = parts[1]
	}
	return m
}

func TestGooseRegistryEntry(t *testing.T) {
	rt, err := Get("goose")
	if err != nil {
		t.Fatalf("Get(goose): %v", err)
	}
	if rt.Type() != "goose" {
		t.Errorf("Type() = %q, want goose", rt.Type())
	}
	if v := rt.CLIVersion(); v != "v1.55.0" {
		t.Errorf("CLIVersion() = %q, want v1.55.0 (spike pin)", v)
	}
	if s := rt.CredentialShape(); s != ShapeAPIKey {
		t.Errorf("CredentialShape() = %q, want %q", s, ShapeAPIKey)
	}
	found := false
	for _, typ := range Registered() {
		if typ == "goose" {
			found = true
		}
	}
	if !found {
		t.Errorf("Registered() missing goose: %v", Registered())
	}
	caps := rt.Capabilities()
	if !caps.Streaming || !caps.ToolCalls || !caps.BYOModelEndpoint {
		t.Errorf("Capabilities() = %+v: streaming, tool calls and BYO endpoint are spike-proven", caps)
	}
	if caps.InteractivePrompt {
		t.Errorf("InteractivePrompt must be false: GOOSE_MODE=auto is headless (spike contract)")
	}
	if caps.RateLimitSignal {
		t.Errorf("RateLimitSignal must stay false until a live 429 capture pins the error frame shape")
	}
}

func TestGooseCommandOllamaLane(t *testing.T) {
	lc := gooseLC()
	lc.ModelRoute = a2a.ModelRoute{Model: "qwen3.5:35b", Endpoint: "http://10.0.0.185:11434/v1"}
	spec, err := gooseRuntime{}.Command(lc)
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	if spec.Path != "goose" {
		t.Fatalf("Path = %q, want goose", spec.Path)
	}
	wantArgs := []string{"run", "-i", "-", "--output-format", "stream-json", "--no-session", "--no-profile"}
	if strings.Join(spec.Args, " ") != strings.Join(wantArgs, " ") {
		t.Errorf("Args = %v, want %v (locked spike surface, no MCP endpoints)", spec.Args, wantArgs)
	}
	if spec.Stdin != "sys ctx\n\ndo the thing" {
		t.Errorf("Stdin = %q, want the coalesced envelope prompt on stdin (NFR-SEC1)", spec.Stdin)
	}
	if spec.WorkDir != "/sandbox/wd" {
		t.Errorf("WorkDir = %q, want /sandbox/wd", spec.WorkDir)
	}
	env := envMap(spec.Env)
	// Spike env contract + the shared BYO wire (mesh- and conformance-
	// consistent: modelRouteEnv's OPENAI_* pair with the placeholder key).
	for k, want := range map[string]string{
		"GOOSE_PROVIDER":        "openai",
		"OPENAI_BASE_URL":       "http://10.0.0.185:11434/v1",
		"OPENAI_API_KEY":        "ollama", // zero-credential lane placeholder
		"GOOSE_MODEL":           "qwen3.5:35b",
		"GOOSE_MODE":            "auto",
		"GOOSE_TELEMETRY_OFF":   "1",
		"GOOSE_DISABLE_KEYRING": "1",
	} {
		if got := env[k]; got != want {
			t.Errorf("env[%s] = %q, want %q", k, got, want)
		}
	}
	if _, ok := env["OLLAMA_HOST"]; ok {
		t.Errorf("OLLAMA_HOST must not be stamped by the engine: the native ollama lane is caller-pinned, the BYO route rides OPENAI_*")
	}
	// State-dir isolation (goose info evidence on v1.55.0; mesh-consistent
	// XDG_CONFIG_HOME convention).
	for k, want := range map[string]string{
		"XDG_CONFIG_HOME": "/sandbox/wd/.config",
		"XDG_DATA_HOME":   "/sandbox/wd/.local/share",
		"XDG_STATE_HOME":  "/sandbox/wd/.local/state",
	} {
		if got := env[k]; got != want {
			t.Errorf("env[%s] = %q, want %q", k, got, want)
		}
	}
	// GOOSE_CONFIG_DIR does not exist on v1.55.0 — the pre-evidence draft
	// used it; guard it never comes back.
	if _, ok := env["GOOSE_CONFIG_DIR"]; ok {
		t.Errorf("GOOSE_CONFIG_DIR must not be set: v1.55.0 ignores it (spike board evidence)")
	}
	if len(spec.WorkDirFiles) != 0 {
		t.Errorf("WorkDirFiles = %v, want none without MCP endpoints", spec.WorkDirFiles)
	}
	if spec.Parse == nil {
		t.Errorf("Parse must be set (stream-json decode)")
	}
	if spec.SettleLine == nil {
		t.Errorf("SettleLine must be set (complete event)")
	}
}

func TestGooseCommandBYOEndpoint(t *testing.T) {
	lc := gooseLC()
	lc.ModelRoute = a2a.ModelRoute{Model: "glm-4.7", Endpoint: "https://api.example.com/v1", Token: "sk-route"}
	spec, err := gooseRuntime{}.Command(lc)
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	env := envMap(spec.Env)
	if env["GOOSE_PROVIDER"] != "openai" {
		t.Errorf("GOOSE_PROVIDER = %q, want openai (BYO OpenAI-compatible lane)", env["GOOSE_PROVIDER"])
	}
	if env["OPENAI_BASE_URL"] != "https://api.example.com/v1" {
		t.Errorf("OPENAI_BASE_URL = %q, want the BYO endpoint", env["OPENAI_BASE_URL"])
	}
	if env["OPENAI_API_KEY"] != "sk-route" {
		t.Errorf("OPENAI_API_KEY = %q, want the route token", env["OPENAI_API_KEY"])
	}
	if env["GOOSE_MODEL"] != "glm-4.7" {
		t.Errorf("GOOSE_MODEL = %q, want ModelRoute.Model precedence", env["GOOSE_MODEL"])
	}
	if _, ok := env["OLLAMA_HOST"]; ok {
		t.Errorf("OLLAMA_HOST must not be set on the authenticated BYO lane")
	}
}

func TestGooseCommandCredentialLane(t *testing.T) {
	lc := gooseLC()
	lc.Credential = "sk-vendor-key"
	spec, err := gooseRuntime{}.Command(lc)
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	env := envMap(spec.Env)
	if env["GOOSE_PROVIDER"] != "openai" {
		t.Errorf("GOOSE_PROVIDER = %q, want openai (api-key credential rides the openai provider)", env["GOOSE_PROVIDER"])
	}
	if env["OPENAI_API_KEY"] != "sk-vendor-key" {
		t.Errorf("OPENAI_API_KEY = %q, want the credential", env["OPENAI_API_KEY"])
	}
	// NFR-SEC1/SEC3: the credential must never ride argv or the stdin prompt.
	if strings.Contains(strings.Join(spec.Args, " "), "sk-vendor-key") {
		t.Errorf("credential leaked into argv")
	}
	if strings.Contains(spec.Stdin, "sk-vendor-key") {
		t.Errorf("credential leaked into the stdin prompt")
	}
}

func TestGooseCommandFailClosed(t *testing.T) {
	spec, err := gooseRuntime{}.Command(gooseLC())
	if err == nil {
		t.Fatalf("Command must fail closed with no credential and no model route (refuses an unconfigured provider), got %+v", spec)
	}
	if !strings.Contains(err.Error(), "refusing") {
		t.Errorf("error should name the fail-closed posture: %v", err)
	}
}

func TestGooseCommandMCPConfig(t *testing.T) {
	lc := gooseLC()
	lc.ModelRoute = a2a.ModelRoute{Model: "qwen3.5:35b", Endpoint: "http://10.0.0.185:11434/v1"}
	lc.MCPEndpoints = []capability.Endpoint{{
		Name:      "github-mcp",
		Transport: "stdio",
		Command:   "npx",
		Args:      []string{"-y", "@modelcontextprotocol/server-github"},
		EnvNames:  []string{"KSQUAD_MCP_GITHUB_MCP_TOKEN"},
	}}
	spec, err := gooseRuntime{}.Command(lc)
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	// v1.55.0 skips config.yaml extensions under --no-profile (verified
	// live), so MCP Runs drop the flag: the rendered extensions must load.
	if strings.Contains(strings.Join(spec.Args, " "), "--no-profile") {
		t.Errorf("Args = %v: MCP Runs must not pass --no-profile (config.yaml extensions would be skipped)", spec.Args)
	}
	wantArgs := []string{"run", "-i", "-", "--output-format", "stream-json", "--no-session"}
	if strings.Join(spec.Args, " ") != strings.Join(wantArgs, " ") {
		t.Errorf("Args = %v, want %v", spec.Args, wantArgs)
	}
	if len(spec.WorkDirFiles) != 1 {
		t.Fatalf("WorkDirFiles = %v, want exactly the goose config.yaml", spec.WorkDirFiles)
	}
	f := spec.WorkDirFiles[0]
	if f.Name != ".config/goose/config.yaml" {
		t.Errorf("config file name = %q, want .config/goose/config.yaml (XDG nesting verified via goose info)", f.Name)
	}
	// The renderer prepends a provenance header (comment lines) — strip
	// them, then decode via yaml (sigs.k8s.io/yaml accepts the JSON-shaped
	// document the renderer emits).
	body := ""
	for _, l := range strings.Split(string(f.Content), "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "#") {
			continue
		}
		body += l + "\n"
	}
	var doc struct {
		Extensions map[string]json.RawMessage `json:"extensions"`
	}
	if err := yaml.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("rendered config is not decodable YAML: %v\n%s", err, f.Content)
	}
	entry, ok := doc.Extensions["github-mcp"]
	if !ok {
		t.Fatalf("github-mcp missing from rendered config:\n%s", f.Content)
	}
	if !strings.Contains(string(entry), "KSQUAD_MCP_GITHUB_MCP_TOKEN") {
		t.Errorf("credential env NAME must be referenced, never literal:\n%s", entry)
	}
}

func TestGooseSettleLine(t *testing.T) {
	tests := []struct {
		line string
		want bool
	}{
		{`    __( O)>  ● new session · ollama qwen3.5:35b`, false},
		{`   \____)    20261018_1 · /sandbox/wd`, false},
		{`     L L     goose is ready`, false},
		{`{"type":"message","message":{"content":[{"type":"text","text":"SPI"}]}}`, false},
		{`{"type":"error","error":{"message":"boom"}}`, false},
		{`{"type":"complete","total_tokens":10955,"input_tokens":10921,"output_tokens":34}`, true},
	}
	for _, tc := range tests {
		if got := gooseSettleLine(tc.line); got != tc.want {
			t.Errorf("gooseSettleLine(%q) = %v, want %v", tc.line, got, tc.want)
		}
	}
}

// gooseToolFixture lines are lifted from the forced tool-loop capture on the
// pinned v1.55.0 binary (developer builtin, shell tool; probe artifacts
// /tmp/goose-toolA on claude-dev, recorded in the ISI-6200 issue comment):
// the wire block vocabulary is thinking/text/toolRequest/toolResponse —
// NOT the Anthropic-style tool_use/tool_result the spike draft assumed.
func gooseToolFixture() []string {
	return []string{
		`    __( O)>  ● new session · ollama qwen3.5:122B`,
		`   \____)    20261018_5 · /sandbox/wd`,
		`     L L     goose is ready`,
		`{"type":"message","message":{"id":"chatcmpl-1","role":"assistant","created":1792332209,"content":[{"type":"thinking","thinking":"The ","signature":""}],"metadata":{"userVisible":true,"agentVisible":true,"inference":{"provider":"ollama","requestedModel":"qwen3.5:122B"},"operations":{"llm":{"advertised_tools":["edit","load_skill","read_image","shell","tree","write"]}}}}}`,
		`{"type":"message","message":{"id":"chatcmpl-1","role":"assistant","created":1792332209,"content":[{"type":"thinking","thinking":"user ","signature":""}],"metadata":{"userVisible":true}}}`,
		`{"type":"message","message":{"id":"chatcmpl-1","role":"assistant","created":1792332209,"content":[{"type":"text","text":"TOOL"}],"metadata":{"userVisible":true}}}`,
		`{"type":"message","message":{"id":"chatcmpl-1","role":"assistant","created":1792332209,"content":[{"type":"text","text":"PROBE"}],"metadata":{"userVisible":true}}}`,
		`{"type":"message","message":{"id":"chatcmpl-1","role":"assistant","created":1792332210,"content":[{"type":"toolRequest","id":"call_f2rxm8op","toolCall":{"status":"success","value":{"name":"shell","arguments":{"command":"echo TOOLPROBE42"}}},"_meta":{"goose.toolCall.providerIndex":0,"goose_extension":"developer"}}],"metadata":{"userVisible":true}}}`,
		`{"type":"message","message":{"id":"chatcmpl-1","role":"assistant","created":1792332211,"content":[{"type":"toolResponse","id":"call_f2rxm8op","toolResult":{"status":"success","value":{"resultType":"complete","content":[{"type":"text","text":"TOOLPROBE42","annotations":{"priority":0.0}}],"structuredContent":{"stdout":"TOOLPROBE42","stderr":"","exit_code":0},"isError":false}}}],"metadata":{"userVisible":true}}}`,
		`{"type":"message","message":{"id":"chatcmpl-1","role":"assistant","created":1792332212,"content":[{"type":"text","text":"done"}],"metadata":{"userVisible":true}}}`,
		`{"type":"complete","total_tokens":24112,"input_tokens":24008,"output_tokens":104,"cache_read_input_tokens":7,"cache_write_input_tokens":3}`,
	}
}

func TestGooseParserBannerSkipAndCoalescing(t *testing.T) {
	parse := newGooseParser()
	var msgs []string
	var usage *a2a.UsagePayload
	for _, line := range gooseToolFixture() {
		for _, p := range parse(line) {
			switch p.Kind {
			case a2a.EventMessage:
				msgs = append(msgs, p.Message.Text)
			case a2a.EventUsage:
				usage = p.Usage
			case a2a.EventTool:
				// covered by TestGooseParserToolLoopVocabulary
			}
		}
	}
	// Banner lines never surface; thinking deltas coalesce to ONE message,
	// text deltas to ONE each per flush point.
	if len(msgs) != 3 {
		t.Fatalf("messages = %v, want [thinking, TOOLPROBE, done] coalesced", msgs)
	}
	if msgs[0] != "The user " {
		t.Errorf("thinking coalesced = %q, want %q", msgs[0], "The user ")
	}
	if msgs[1] != "TOOLPROBE" {
		t.Errorf("text coalesced = %q, want %q", msgs[1], "TOOLPROBE")
	}
	if msgs[2] != "done" {
		t.Errorf("post-tool text = %q, want %q", msgs[2], "done")
	}
	if usage == nil {
		t.Fatalf("complete event must surface usage")
	}
	if usage.Input != 24008 || usage.Output != 104 {
		t.Errorf("usage tokens = %d/%d, want 24008/104 (captured complete event)", usage.Input, usage.Output)
	}
	if usage.CacheRead != 7 || usage.CacheWrite != 3 {
		t.Errorf("cache tokens = %d/%d, want 7/3 (v1.55.0 wire carries cache_read/write_input_tokens)", usage.CacheRead, usage.CacheWrite)
	}
	if usage.Reasoning != 0 {
		t.Errorf("reasoning must not be fabricated: total = input+output(+cache) on the captured wire")
	}
}

func TestGooseParserToolLoopVocabulary(t *testing.T) {
	parse := newGooseParser()
	var tools []*a2a.ToolPayload
	var toolArgs []string
	for _, line := range gooseToolFixture() {
		for _, p := range parse(line) {
			if p.Kind == a2a.EventTool {
				tools = append(tools, p.Tool)
				if p.ToolArgs != "" {
					toolArgs = append(toolArgs, p.ToolArgs)
				}
			}
		}
	}
	if len(tools) != 2 {
		t.Fatalf("tool events = %d, want 2 (start + result)", len(tools))
	}
	start, result := tools[0], tools[1]
	if start.Name != "shell" || start.Phase != "start" {
		t.Errorf("start event = %+v, want shell/start", start)
	}
	if start.Server != "developer" {
		t.Errorf("start.Server = %q, want developer (_meta.goose_extension → ToolPayload.Server)", start.Server)
	}
	if len(toolArgs) != 1 || !strings.Contains(toolArgs[0], `"echo TOOLPROBE42"`) {
		t.Errorf("raw tool args must ride the internal seam, got %v", toolArgs)
	}
	if result.Name != "shell" || result.Phase != "result" {
		t.Errorf("result event = %+v, want shell/result correlated by call id", result)
	}
	if result.Server != "developer" {
		t.Errorf("result.Server = %q, want developer (correlated from the request)", result.Server)
	}
	if result.OK == nil || !*result.OK {
		t.Errorf("result.OK = %v, want true (isError=false on the captured wire)", result.OK)
	}
}

func TestGooseParserToolErrorResult(t *testing.T) {
	parse := newGooseParser()
	req := `{"type":"message","message":{"content":[{"type":"toolRequest","id":"call_err1","toolCall":{"status":"success","value":{"name":"shell","arguments":{"command":"false"}}},"_meta":{"goose_extension":"developer"}}]}}`
	res := `{"type":"message","message":{"content":[{"type":"toolResponse","id":"call_err1","toolResult":{"status":"success","value":{"resultType":"complete","content":[{"type":"text","text":"command failed"}],"isError":true}}}]}}`
	var tools []*a2a.ToolPayload
	for _, p := range parse(req) {
		if p.Kind == a2a.EventTool {
			tools = append(tools, p.Tool)
		}
	}
	for _, p := range parse(res) {
		if p.Kind == a2a.EventTool {
			tools = append(tools, p.Tool)
		}
	}
	if len(tools) != 2 {
		t.Fatalf("tool events = %d, want 2", len(tools))
	}
	if tools[1].OK == nil || *tools[1].OK {
		t.Errorf("error result OK = %v, want false (isError=true)", tools[1].OK)
	}
	if tools[1].Summary == "" {
		t.Errorf("error result must carry a summary from the result text blocks")
	}
}

func TestGooseParserUnmatchedToolResponse(t *testing.T) {
	// A toolResponse whose toolRequest was never seen (stream started
	// mid-loop) must not crash and must still surface a result event.
	parse := newGooseParser()
	line := `{"type":"message","message":{"content":[{"type":"toolResponse","id":"call_unknown","toolResult":{"status":"success","value":{"resultType":"complete","content":[{"type":"text","text":"late"}],"isError":false}}}]}}`
	events := parse(line)
	if len(events) != 1 || events[0].Kind != a2a.EventTool {
		t.Fatalf("events = %+v, want one orphan tool result", events)
	}
	tool := events[0].Tool
	if tool.Name != "" || tool.Phase != "result" {
		t.Errorf("orphan result = %+v, want empty name (unknowable) + result phase", tool)
	}
}

func TestGooseParserErrorFrameDefensive(t *testing.T) {
	parse := newGooseParser()
	line := `{"type":"error","error":{"message":"Rate limit exceeded (429): too many requests"}}`
	events := parse(line)
	kinds := []a2a.EventType{}
	for _, p := range events {
		kinds = append(kinds, p.Kind)
	}
	want := []a2a.EventType{a2a.EventMessage, a2a.EventUsage, a2a.EventRateLimited}
	if len(kinds) != len(want) {
		t.Fatalf("event kinds = %v, want %v (message + usage-error + rate-limit signature)", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("event kinds = %v, want %v", kinds, want)
		}
	}
}

func TestGooseParserPostJSONNonJSONDegrades(t *testing.T) {
	parse := newGooseParser()
	parse(`{"type":"message","message":{"content":[{"type":"text","text":"a"}]}}`)
	events := parse(`not json anymore`)
	if len(events) != 1 || events[0].Kind != a2a.EventMessage || events[0].Message.Text != "not json anymore" {
		t.Fatalf("post-stream non-JSON must degrade to an opaque message event, got %+v", events)
	}
}
