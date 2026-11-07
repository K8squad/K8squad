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
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	wire "github.com/K8squad/K8squad/pkg/a2a"
)

// mirrorFixture is a ProgressMirror with the SQL seams stubbed: lookup returns
// a fixed work item, append records (and can fail on demand).
type mirrorFixture struct {
	m           *ProgressMirror
	mu          sync.Mutex
	got         []string            // "workItem|author|body" per appended progress row
	summaries   []string            // "workItem|author|runID|body" per deliverable summary (ISI-7099)
	published   []publishedThinking // liveness echoes (ISI-5193), in order
	failOn      int                 // append calls >= failOn fail (0 = never)
	failSummary int                 // appendSummary calls >= failSummary fail (0 = never)
	calls       int
	sumCalls    int
}

// publishedThinking records one liveness-echo publish for assertions.
type publishedThinking struct {
	workItemID string
	runID      string
	eventType  string
	payload    []byte
}

func newMirrorFixture(t *testing.T) *mirrorFixture {
	t.Helper()
	f := &mirrorFixture{}
	db, err := sql.Open("pgx", "host=localhost port=1 dbname=unused connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	m := &ProgressMirror{
		db:          db,
		minInterval: 0, // tests control the flood guard explicitly
		pubInterval: 0, // …and the ephemeral thinking pacing (ISI-7099)
		lastSeq:     make(map[string]uint64),
		lastAt:      make(map[string]time.Time),
		lastPubAt:   make(map[string]time.Time),
		delivered:   make(map[string]bool),
	}
	m.lookup = func(context.Context, string) (string, error) {
		return "11111111-1111-1111-1111-111111111111", nil
	}
	m.append = func(_ context.Context, workItemID, author, runID, body string) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls++
		if f.failOn != 0 && f.calls >= f.failOn {
			return errors.New("append boom")
		}
		f.got = append(f.got, workItemID+"|"+author+"|"+body)
		return nil
	}
	m.appendSummary = func(_ context.Context, workItemID, author, runID, body string) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.sumCalls++
		if f.failSummary != 0 && f.sumCalls >= f.failSummary {
			return errors.New("appendSummary boom")
		}
		f.summaries = append(f.summaries, workItemID+"|"+author+"|"+runID+"|"+body)
		return nil
	}
	m.publish = func(_ context.Context, workItemID, runID, eventType string, payload []byte) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.published = append(f.published, publishedThinking{workItemID, runID, eventType, append([]byte(nil), payload...)})
		return nil
	}
	f.m = m
	return f
}

func (f *mirrorFixture) echoes() []publishedThinking {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]publishedThinking(nil), f.published...)
}

func (f *mirrorFixture) comments() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.got...)
}

func (f *mirrorFixture) summariesList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.summaries...)
}

// deliverableEv is a WS-1-tagged final-answer message (ISI-7098 kind on the
// wire — what ISI-7099's comment tier is built from).
func deliverableEv(seq uint64, task, text string) wire.Event {
	return wire.Event{Seq: seq, A2ATaskID: task, Type: wire.EventMessage,
		Payload: wire.MessagePayload{Role: "agent", Kind: wire.MessageKindDeliverable, Text: text, Trust: "untrusted"}}
}

func msgEv(seq uint64, task, text string) wire.Event {
	return wire.Event{Seq: seq, A2ATaskID: task, Type: wire.EventMessage,
		Payload: wire.MessagePayload{Role: "agent", Text: text, Trust: "untrusted"}}
}

func toolEv(seq uint64, task, name, phase, summary string) wire.Event {
	return wire.Event{Seq: seq, A2ATaskID: task, Type: wire.EventTool,
		Payload: wire.ToolPayload{Name: name, Phase: phase, Summary: summary}}
}

func statusEv(seq uint64, task string, state wire.TaskState) wire.Event {
	return wire.Event{Seq: seq, A2ATaskID: task, Type: wire.EventStatus,
		Payload: wire.StatusPayload{State: state}}
}

// TestProgressMirrorWritesIncrementalComments is the ISI-4235 AC, rescoped
// by ISI-7099: the DURABLE progress surface (usage digests, terminal status)
// still lands incrementally as kind='progress' Activity rows while the run
// executes — while thinking text and tool calls are EPHEMERAL (echoed to the
// live run SSE only, zero durable rows).
func TestProgressMirrorWritesIncrementalComments(t *testing.T) {
	f := newMirrorFixture(t)
	ctx := context.Background()
	task := "9952db54-0000-0000-0000-000000000000"

	for _, ev := range []wire.Event{
		msgEv(1, task, "Reading the repo layout…"),
		toolEv(2, task, "shell", "start", "ls -la"),
		{Seq: 3, A2ATaskID: task, Type: wire.EventUsage,
			Payload: wire.UsagePayload{Model: "qwen", Input: 10, Output: 5}},
		statusEv(4, task, wire.TaskCompleted),
	} {
		if err := f.m.Event(ctx, ev); err != nil {
			t.Fatalf("Event(seq %d) errored: %v (progress must never abort the run)", ev.Seq, err)
		}
	}

	// Ephemeral tier: message + tool produce NO durable rows, only echoes.
	got := f.comments()
	if len(got) != 2 {
		t.Fatalf("appended %d durable rows, want 2 (usage + terminal status): %v", len(got), got)
	}
	if !strings.Contains(got[0], "[llm:response] qwen") {
		t.Fatalf("usage digest row wrong: %q", got[0])
	}
	if !strings.Contains(got[1], "[status] completed") {
		t.Fatalf("terminal status row wrong: %q", got[1])
	}
	for _, c := range got {
		parts := strings.SplitN(c, "|", 3)
		if parts[0] != "11111111-1111-1111-1111-111111111111" {
			t.Fatalf("row not on the dispatch-marker work item: %q", c)
		}
		if parts[1] != "run/9952db54" {
			t.Fatalf("author not run-provenanced: %q", parts[1])
		}
		if !strings.HasPrefix(parts[2], "[run 9952db54]") {
			t.Fatalf("body lacks the short-run prefix: %q", parts[2])
		}
	}
	echoes := f.echoes()
	if len(echoes) != 4 {
		t.Fatalf("published %d echoes, want 4 (2 ephemeral + 2 durable): %d", len(echoes), len(echoes))
	}
	if !strings.Contains(string(echoes[0].payload), "Reading the repo layout") ||
		!strings.Contains(string(echoes[1].payload), "[tool:shell/start]") {
		t.Fatalf("ephemeral echoes missing thinking/tool narration: %+v", echoes[:2])
	}
}

// TestProgressMirrorAuthorsAgentIdentity is the ISI-4739 AC: when the work
// item's checkout claim names an agent, the run's rows are authored
// agent:<name> (so the console renders the NAME and hashes a stable per-agent
// colour, ISI-4706); when the assignee can't be resolved the author falls
// back to run/<shortRunID>. Provenance rides the author either way (ISI-7099:
// the deliverable summary carries it too).
func TestProgressMirrorAuthorsAgentIdentity(t *testing.T) {
	f := newMirrorFixture(t)
	f.m.assignee = func(context.Context, string) string { return "sam" }
	ctx := context.Background()
	task := "c3beee63-0000-0000-0000-000000000000"

	if err := f.m.Event(ctx, deliverableEv(1, task, "Shipped the fix.")); err != nil {
		t.Fatalf("Event errored: %v", err)
	}
	got := f.summariesList()
	if len(got) != 1 {
		t.Fatalf("appended %d summaries, want 1: %v", len(got), got)
	}
	parts := strings.SplitN(got[0], "|", 4)
	if parts[1] != "agent:sam" {
		t.Fatalf("author = %q, want agent:sam", parts[1])
	}
	if parts[3] != "Shipped the fix." {
		t.Fatalf("deliverable body must be clean prose: %q", parts[3])
	}

	// Empty assignee → fall back to run/<shortRunID> (no claim row / released).
	f.m.assignee = func(context.Context, string) string { return "" }
	f.m.agentCache.Range(func(k, _ any) bool { f.m.agentCache.Delete(k); return true })
	task2 := "d4ffff74-0000-0000-0000-000000000000"
	if err := f.m.Event(ctx, deliverableEv(1, task2, "no agent")); err != nil {
		t.Fatalf("Event errored: %v", err)
	}
	got = f.summariesList()
	if parts := strings.SplitN(got[len(got)-1], "|", 4); parts[1] != "run/d4ffff74" {
		t.Fatalf("fallback author = %q, want run/d4ffff74", parts[1])
	}
}

// TestProgressMirrorCachesAssignee: the assignee lookup is resolved once per
// work item and cached — chatty runs don't re-query coord.claim per row.
func TestProgressMirrorCachesAssignee(t *testing.T) {
	f := newMirrorFixture(t)
	f.m.minInterval = 0
	var calls int
	f.m.assignee = func(context.Context, string) string { calls++; return "kai" }
	ctx := context.Background()
	task := "e5aaaa85-0000-0000-0000-000000000000"

	for seq := uint64(1); seq <= 3; seq++ {
		_ = f.m.Event(ctx, msgEv(seq, task, "tick"))
	}
	if calls != 1 {
		t.Fatalf("assignee resolved %d times, want 1 (cached)", calls)
	}
	echoes := f.echoes()
	if len(echoes) != 3 {
		t.Fatalf("published %d echoes, want 3", len(echoes))
	}
	for _, e := range echoes {
		var p thinkingPayload
		if err := json.Unmarshal(e.payload, &p); err != nil {
			t.Fatalf("echo payload not valid JSON: %v", err)
		}
		if p.Author != "agent:kai" {
			t.Fatalf("echo author = %q, want agent:kai", p.Author)
		}
	}
	if got := f.comments(); len(got) != 0 {
		t.Fatalf("thinking ticks must not append durable rows (ISI-7099): %v", got)
	}
}

// TestProgressMirrorEnrichesToolEnvelope is the ISI-5191 AC, carried on the
// ephemeral live tier (ISI-7099): the tool row carries Command/Skill/Server
// as stable bracketed segments in its thinking echo — and the raw call
// arguments are NEVER emitted, only ever available as the emitter's
// pre-hashed ArgsSHA256 digest.
func TestProgressMirrorEnrichesToolEnvelope(t *testing.T) {
	f := newMirrorFixture(t)
	ctx := context.Background()
	task := "aa11bb22-0000-0000-0000-000000000000"

	// A bash-wrapped git call served locally: Command enriches the opaque
	// "shell" name; no MCP server. The raw args ("commit -m secret…") are
	// hashed by the emitter — only the digest rides ArgsSHA256, never the body.
	f.m.minInterval = 0
	ev1 := wire.Event{Seq: 1, A2ATaskID: task, Type: wire.EventTool,
		Payload: wire.ToolPayload{Name: "shell", Phase: "start", Command: "git",
			Skill: "commit-flow", Summary: "staging changes",
			ArgsSHA256: "deadbeefcafef00d"}}
	// An MCP-served tool call: Server present, no Command.
	ok := true
	ev2 := wire.Event{Seq: 2, A2ATaskID: task, Type: wire.EventTool,
		Payload: wire.ToolPayload{Name: "search", Phase: "result", OK: &ok,
			Server: "context7", Summary: "3 hits"}}
	for _, ev := range []wire.Event{ev1, ev2} {
		if err := f.m.Event(ctx, ev); err != nil {
			t.Fatalf("Event(seq %d) errored: %v", ev.Seq, err)
		}
	}
	echoes := f.echoes()
	if len(echoes) != 2 {
		t.Fatalf("published %d echoes, want 2: %+v", len(echoes), echoes)
	}
	if got := f.comments(); len(got) != 0 {
		t.Fatalf("tool calls are ephemeral — no durable rows (ISI-7099): %v", got)
	}
	bodies := make([]string, 2)
	for i, e := range echoes {
		var p thinkingPayload
		if err := json.Unmarshal(e.payload, &p); err != nil {
			t.Fatalf("echo %d payload not valid JSON: %v", i, err)
		}
		bodies[i] = p.Body
	}
	body1 := bodies[0]
	if !strings.Contains(body1, "[tool:shell/start]") ||
		!strings.Contains(body1, "[cmd:git]") ||
		!strings.Contains(body1, "[skill:commit-flow]") ||
		!strings.Contains(body1, "staging changes") {
		t.Fatalf("tool row not enriched with command/skill/summary: %q", body1)
	}
	if strings.Contains(body1, "[server:") {
		t.Fatalf("local tool row must carry no server segment: %q", body1)
	}
	if strings.Contains(body1, "deadbeefcafef00d") || strings.Contains(body1, "secret") {
		t.Fatalf("args digest / raw args leaked onto the wire body: %q", body1)
	}
	body2 := bodies[1]
	if !strings.Contains(body2, "[tool:search/result(ok)]") ||
		!strings.Contains(body2, "[server:context7]") ||
		!strings.Contains(body2, "3 hits") {
		t.Fatalf("MCP tool row not enriched with server/summary: %q", body2)
	}
	if strings.Contains(body2, "[cmd:") || strings.Contains(body2, "[skill:") {
		t.Fatalf("MCP tool row must carry no command/skill segment: %q", body2)
	}
}

// TestProgressMirrorRaisedBodyCap is the ISI-5191 AC: the narration/summary
// cap was lifted well past the original 480 runes so real detail survives.
// The cap still bounds a runaway runtime message — now on the ephemeral echo
// and on the deliverable summary alike (ISI-7099).
func TestProgressMirrorRaisedBodyCap(t *testing.T) {
	if progressMirrorMaxBody <= 480 {
		t.Fatalf("body cap not raised past the original 480: got %d", progressMirrorMaxBody)
	}
	f := newMirrorFixture(t)
	f.m.minInterval = 0
	ctx := context.Background()
	task := "bb22cc33-0000-0000-0000-000000000000"

	// A 2 KiB narration (well past the old 480 clip) must survive intact.
	detail := strings.Repeat("y", 2000)
	if err := f.m.Event(ctx, msgEv(1, task, detail)); err != nil {
		t.Fatalf("Event errored: %v", err)
	}
	echoes := f.echoes()
	if len(echoes) != 1 {
		t.Fatalf("published %d echoes, want 1", len(echoes))
	}
	var p thinkingPayload
	if err := json.Unmarshal(echoes[0].payload, &p); err != nil {
		t.Fatalf("echo payload not valid JSON: %v", err)
	}
	if !strings.Contains(p.Body, detail) {
		t.Fatalf("2 KiB narration was clipped under the raised cap: len(body)=%d", len(p.Body))
	}
	if strings.Contains(p.Body, "…") {
		t.Fatalf("narration within the raised cap must not be truncated: %q", p.Body[:64])
	}
}

// TestProgressMirrorDedupsReplayedSeqs: the EventSink contract is
// at-least-once — a replayed/duplicate seq must not double-emit.
func TestProgressMirrorDedupsReplayedSeqs(t *testing.T) {
	f := newMirrorFixture(t)
	ctx := context.Background()
	task := "r1"

	_ = f.m.Event(ctx, msgEv(5, task, "first"))
	_ = f.m.Event(ctx, msgEv(5, task, "first (replayed)")) // same seq
	_ = f.m.Event(ctx, msgEv(4, task, "older seq"))        // stale seq
	if got := f.echoes(); len(got) != 1 {
		t.Fatalf("dedup failed: %d echoes, want 1: %+v", len(got), got)
	}
	_ = f.m.Event(ctx, msgEv(6, task, "next"))
	if got := f.echoes(); len(got) != 2 {
		t.Fatalf("in-order advance failed: %d echoes, want 2", len(got))
	}
}

// TestProgressMirrorFloodGuardDropsButTerminalLands: events faster than the
// per-run pacing windows are dropped, the terminal status event always
// lands, and — the ISI-7099 correctness pin — the deliverable is NEVER
// subject to any guard.
func TestProgressMirrorFloodGuardDropsButTerminalLands(t *testing.T) {
	f := newMirrorFixture(t)
	f.m.minInterval = time.Hour // stretch both windows: everything guarded drops
	f.m.pubInterval = time.Hour
	ctx := context.Background()
	task := "r2"

	_ = f.m.Event(ctx, msgEv(1, task, "one"))
	_ = f.m.Event(ctx, msgEv(2, task, "two (within window — dropped)"))
	_ = f.m.Event(ctx, toolEv(3, task, "shell", "start", "also dropped"))
	if got := f.echoes(); len(got) != 1 {
		t.Fatalf("ephemeral pacing failed: %d echoes, want 1: %+v", len(got), got)
	}
	_ = f.m.Event(ctx, statusEv(4, task, wire.TaskCompleted))
	got := f.comments()
	if len(got) != 1 || !strings.Contains(got[0], "[status] completed") {
		t.Fatalf("terminal event must bypass the flood guard: %v", got)
	}
	// The deliverable bypasses every guard (ISI-7099: today the final answer
	// is droppable — that latent bug is the reason it is exempt).
	_ = f.m.Event(ctx, deliverableEv(5, task, "the answer"))
	if s := f.summariesList(); len(s) != 1 || !strings.Contains(s[0], "the answer") {
		t.Fatalf("deliverable must never be flood-dropped: %v", s)
	}
}

// TestProgressMirrorSwallowsOwnFailures: a failing append (or lookup) must
// never surface — a sink error cancels the live task (§5.1), and progress
// mirroring is strictly best-effort.
func TestProgressMirrorSwallowsOwnFailures(t *testing.T) {
	f := newMirrorFixture(t)
	f.failOn = 1 // every append fails
	ctx := context.Background()

	if err := f.m.Event(ctx, msgEv(1, "r3", "text")); err != nil {
		t.Fatalf("append failure surfaced: %v", err)
	}
	// Lookup failure path: rebind lookup to error, new task id.
	f.m.lookup = func(context.Context, string) (string, error) {
		return "", errors.New("db gone")
	}
	if err := f.m.Event(ctx, msgEv(1, "r4", "text")); err != nil {
		t.Fatalf("lookup failure surfaced: %v", err)
	}
	// Unresolved work item (marker absent): dropped silently.
	f.m.lookup = func(context.Context, string) (string, error) { return "", nil }
	if err := f.m.Event(ctx, msgEv(1, "r5", "text")); err != nil {
		t.Fatalf("missing-marker failure surfaced: %v", err)
	}
}

// TestProgressMirrorPublishesLivenessEcho is the ISI-5193 AC, rescoped by
// ISI-7099: every mirrored row — durable or ephemeral — is echoed as a
// run-entity liveness event carrying (author, body) and the run id, so the
// per-run SSE hub can live-append it. The echo fires once per row, in
// order, tagged thinking, with the FULL (unshortened) run id so the projector
// can key its fan-out and the client can dedup against the reloaded thread.
func TestProgressMirrorPublishesLivenessEcho(t *testing.T) {
	f := newMirrorFixture(t)
	f.m.assignee = func(context.Context, string) string { return "sam" }
	ctx := context.Background()
	task := "9952db54-1111-2222-3333-444455556666"

	for _, ev := range []wire.Event{
		msgEv(1, task, "Reading the repo layout…"),
		toolEv(2, task, "shell", "start", "ls -la"),
	} {
		if err := f.m.Event(ctx, ev); err != nil {
			t.Fatalf("Event(seq %d) errored: %v", ev.Seq, err)
		}
	}

	echoes := f.echoes()
	if len(echoes) != 2 {
		t.Fatalf("published %d echoes, want 2", len(echoes))
	}
	if got := f.comments(); len(got) != 0 {
		t.Fatalf("ephemeral tier must not append durable rows (ISI-7099): %v", got)
	}
	for i, e := range echoes {
		if e.eventType != thinkingEventType {
			t.Fatalf("echo %d event type = %q, want %q", i, e.eventType, thinkingEventType)
		}
		if e.runID != task {
			t.Fatalf("echo %d run id = %q, want full run id %q (short id keys no fan-out)", i, e.runID, task)
		}
		if e.workItemID != "11111111-1111-1111-1111-111111111111" {
			t.Fatalf("echo %d work item = %q, want the dispatch-marker item", i, e.workItemID)
		}
		var p thinkingPayload
		if err := json.Unmarshal(e.payload, &p); err != nil {
			t.Fatalf("echo %d payload not valid JSON: %v", i, err)
		}
		if p.Author != "agent:sam" {
			t.Fatalf("echo %d author = %q, want agent:sam", i, p.Author)
		}
		if p.RunID != task || p.At == "" {
			t.Fatalf("echo %d payload missing runId/at: %+v", i, p)
		}
	}
	if !strings.Contains(string(echoes[0].payload), "Reading the repo layout") ||
		!strings.Contains(string(echoes[1].payload), "[tool:shell/start]") {
		t.Fatalf("echo bodies wrong: %+v", echoes)
	}
}

// TestProgressMirrorNoEchoWhenCommentFails: the liveness echo is strictly
// secondary to the DURABLE row — if the progress/summary write fails there
// is no row to be live about, so no run event is published (the reload thread
// is the source of truth). The ephemeral thinking tier has no durable row to
// guard, so its echo fires without one (ISI-7099).
func TestProgressMirrorNoEchoWhenCommentFails(t *testing.T) {
	f := newMirrorFixture(t)
	ctx := context.Background()
	task := "9952db54-0000-0000-0000-000000000000"

	f.failOn = 1
	_ = f.m.Event(ctx, wire.Event{Seq: 1, A2ATaskID: task, Type: wire.EventUsage,
		Payload: wire.UsagePayload{Model: "qwen", Input: 1, Output: 1}})
	if echoes := f.echoes(); len(echoes) != 0 {
		t.Fatalf("published %d echoes despite a failed progress write, want 0", len(echoes))
	}

	f.failSummary = 1
	_ = f.m.Event(ctx, deliverableEv(2, task, "answer"))
	if echoes := f.echoes(); len(echoes) != 0 {
		t.Fatalf("published %d echoes despite a failed summary write, want 0", len(echoes))
	}

	// Ephemeral thinking has no durable dependency — the echo fires anyway.
	_ = f.m.Event(ctx, msgEv(3, task, "thinking aloud"))
	if echoes := f.echoes(); len(echoes) != 1 {
		t.Fatalf("published %d echoes after thinking, want 1 (ephemeral)", len(echoes))
	}
}

// TestProgressMirrorSkipsNoiseAndTruncates: signal-less usage/skill-load/
// non-terminal status events emit nothing (neither row nor echo); oversized
// text is truncated.
func TestProgressMirrorSkipsNoiseAndTruncates(t *testing.T) {
	f := newMirrorFixture(t)
	ctx := context.Background()
	task := "r6"

	// A usage event with neither a model nor any tokens carries no digest
	// (ISI-5192): honest empty, mirrored as nothing.
	_ = f.m.Event(ctx, wire.Event{Seq: 1, A2ATaskID: task, Type: wire.EventUsage,
		Payload: wire.UsagePayload{}})
	_ = f.m.Event(ctx, statusEv(2, task, wire.TaskWorking))
	_ = f.m.Event(ctx, msgEv(3, task, ""))
	if got := f.comments(); len(got) != 0 {
		t.Fatalf("noise events mirrored: %v", got)
	}
	if got := f.echoes(); len(got) != 0 {
		t.Fatalf("noise events echoed: %+v", got)
	}

	long := strings.Repeat("x", progressMirrorMaxBody+50)
	_ = f.m.Event(ctx, msgEv(4, task, long))
	echoes := f.echoes()
	if len(echoes) != 1 {
		t.Fatalf("truncation case: %d echoes, want 1", len(echoes))
	}
	var p thinkingPayload
	if err := json.Unmarshal(echoes[0].payload, &p); err != nil {
		t.Fatalf("echo payload not valid JSON: %v", err)
	}
	if want := progressMirrorMaxBody + len("[run r6][untrusted] ") + len("…"); len(p.Body) != want {
		t.Fatalf("truncated body len = %d, want %d", len(p.Body), want)
	}
}

// TestProgressMirrorMirrorsResponseDigest is the ISI-5192 AC: an EventUsage —
// the same signal that projects the CR LLMInteraction "response" onto
// Run.Status — mirrors a parseable `[llm:response]` digest (model + bounded
// token counts) into the ticket feed, so the ticket agent-turn card can show a
// model round-trip completed. Never the response TEXT (that is ISI-4812).
func TestProgressMirrorMirrorsResponseDigest(t *testing.T) {
	f := newMirrorFixture(t)
	ctx := context.Background()
	task := "r9"

	_ = f.m.Event(ctx, wire.Event{Seq: 1, A2ATaskID: task, Type: wire.EventUsage,
		Payload: wire.UsagePayload{Model: "qwen", Input: 100, Output: 23}})
	got := f.comments()
	if len(got) != 1 {
		t.Fatalf("usage digest: %d comments, want 1: %v", len(got), got)
	}
	body := strings.SplitN(got[0], "|", 3)[2]
	if body != "[run r9][llm:response] qwen · 123 tok (100 in / 23 out)" {
		t.Fatalf("digest body = %q", body)
	}

	// An empty-model runtime (ISI-4412) still reports token-bearing usage: the
	// digest falls back to a generic model label rather than dropping the signal.
	_ = f.m.Event(ctx, wire.Event{Seq: 2, A2ATaskID: task, Type: wire.EventUsage,
		Payload: wire.UsagePayload{Input: 5, Output: 0}})
	got = f.comments()
	if len(got) != 2 {
		t.Fatalf("empty-model digest: %d comments, want 2: %v", len(got), got)
	}
	if body := strings.SplitN(got[1], "|", 3)[2]; body != "[run r9][llm:response] model · 5 tok (5 in / 0 out)" {
		t.Fatalf("empty-model digest body = %q", body)
	}
}

// TestProgressMirrorGenericJSONPayloads: the stdio/NDJSON transports decode
// payloads as generic JSON maps — the mirror must recover the typed shapes,
// including the WS-1 deliverable kind (ISI-7098/ISI-7099).
func TestProgressMirrorGenericJSONPayloads(t *testing.T) {
	f := newMirrorFixture(t)
	ctx := context.Background()
	task := "r7"

	_ = f.m.Event(ctx, wire.Event{Seq: 1, A2ATaskID: task, Type: wire.EventMessage,
		Payload: map[string]any{"role": "agent", "text": "decoded from json", "trust": "untrusted"}})
	_ = f.m.Event(ctx, wire.Event{Seq: 2, A2ATaskID: task, Type: wire.EventTool,
		Payload: map[string]any{"name": "read", "phase": "result", "summary": "file.go"}})
	echoes := f.echoes()
	if len(echoes) != 2 {
		t.Fatalf("json-map payloads: %d echoes, want 2: %+v", len(echoes), echoes)
	}
	if got := f.comments(); len(got) != 0 {
		t.Fatalf("json-map thinking must stay ephemeral (ISI-7099): %v", got)
	}
	if !strings.Contains(string(echoes[0].payload), "decoded from json") ||
		!strings.Contains(string(echoes[1].payload), "[tool:read/result]") {
		t.Fatalf("json-map bodies wrong: %+v", echoes)
	}

	// A JSON-map deliverable decodes its kind and lands as the ONE summary.
	_ = f.m.Event(ctx, wire.Event{Seq: 3, A2ATaskID: task, Type: wire.EventMessage,
		Payload: map[string]any{"role": "agent", "text": "final via json", "kind": "deliverable"}})
	if s := f.summariesList(); len(s) != 1 || !strings.Contains(s[0], "final via json") {
		t.Fatalf("json-map deliverable must land as the one summary: %v", s)
	}
}

// TestProgressMirrorConcurrentRuns: one shared mirror instance serves every
// followed Run — concurrent event streams must not interleave their dedup
// state incorrectly (goes-with-race). Uses durable usage rows so assertions
// do not race the relaxed ephemeral pacing (ISI-7099).
func TestProgressMirrorConcurrentRuns(t *testing.T) {
	f := newMirrorFixture(t)
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		task := "rt" + string(rune('a'+i))
		wg.Add(1)
		go func() {
			defer wg.Done()
			for seq := 1; seq <= 25; seq++ {
				_ = f.m.Event(ctx, wire.Event{Seq: uint64(seq), A2ATaskID: task, Type: wire.EventUsage,
					Payload: wire.UsagePayload{Model: "qwen", Input: seq, Output: 1}})
			}
		}()
	}
	wg.Wait()
	// The exact count is timing-dependent, so assert only a non-empty, deduped
	// result: every appended body carries its own run prefix and no task wrote
	// more than 25.
	got := f.comments()
	if len(got) == 0 {
		t.Fatal("no comments appended under concurrency")
	}
	seen := map[string]int{}
	for _, c := range got {
		parts := strings.SplitN(c, "|", 3)
		if len(parts) != 3 {
			t.Fatalf("comment not workItem|author|body: %q", c)
		}
		prefix := strings.TrimSuffix(strings.TrimPrefix(parts[2], "[run "), "]…")
		prefix = strings.SplitN(prefix, "]", 2)[0]
		seen[prefix]++
		if seen[prefix] > 25 {
			t.Fatalf("run %s appended %d comments — dedup broken", prefix, seen[prefix])
		}
	}
}

// TestProgressMirrorDeliverableIsTheOneComment is the ISI-7099 AC: the run's
// WS-1 deliverable-tagged final message becomes the thread's ONE durable
// comment — kind='summary' via the sanctioned append path, clean body, run-id
// stamped — while every preceding message/tool event stays ephemeral, and a
// second deliverable never yields a second comment.
func TestProgressMirrorDeliverableIsTheOneComment(t *testing.T) {
	f := newMirrorFixture(t)
	ctx := context.Background()
	task := "ff44aa66-0000-0000-0000-000000000000"

	for _, ev := range []wire.Event{
		msgEv(1, task, "thinking about it…"),
		toolEv(2, task, "shell", "start", "go test ./..."),
		msgEv(3, task, "still working"),
	} {
		_ = f.m.Event(ctx, ev)
	}
	if got := f.comments(); len(got) != 0 {
		t.Fatalf("thinking/tool events must stay ephemeral: %v", got)
	}
	if err := f.m.Event(ctx, deliverableEv(4, task, "Done: split shipped, all green.")); err != nil {
		t.Fatalf("Event errored: %v", err)
	}
	s := f.summariesList()
	if len(s) != 1 {
		t.Fatalf("deliverable appended %d summaries, want exactly 1: %v", len(s), s)
	}
	parts := strings.SplitN(s[0], "|", 4)
	if parts[0] != "11111111-1111-1111-1111-111111111111" || parts[2] != task {
		t.Fatalf("summary not stamped workItem/runID: %q", s[0])
	}
	if parts[3] != "Done: split shipped, all green." {
		t.Fatalf("summary body must be the clean deliverable text: %q", parts[3])
	}

	// A second deliverable (higher seq, same run) is demoted to the ephemeral
	// tier — exactly-one holds structurally, never on parser goodwill.
	_ = f.m.Event(ctx, deliverableEv(5, task, "late duplicate answer"))
	if s = f.summariesList(); len(s) != 1 {
		t.Fatalf("second deliverable appended another summary: %v", s)
	}
	if got := f.comments(); len(got) != 0 {
		t.Fatalf("progress rows unexpected: %v", got)
	}

	// The deliverable is echoed live too (in-order stream), with the SAME
	// clean body the durable row carries.
	var deliverableEcho *thinkingPayload
	for _, e := range f.echoes() {
		var p thinkingPayload
		if err := json.Unmarshal(e.payload, &p); err != nil {
			t.Fatalf("echo payload not valid JSON: %v", err)
		}
		if p.Body == "Done: split shipped, all green." {
			deliverableEcho = &p
		}
	}
	if deliverableEcho == nil {
		t.Fatal("deliverable echo missing the clean body")
	}
}
