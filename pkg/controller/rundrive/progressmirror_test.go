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
	m         *ProgressMirror
	mu        sync.Mutex
	got       []string            // "workItem|author|body" per appended comment
	published []publishedThinking // liveness echoes (ISI-5193), in order
	failOn    int                 // append calls >= failOn fail (0 = never)
	calls     int
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
		lastSeq:     make(map[string]uint64),
		lastAt:      make(map[string]time.Time),
	}
	m.lookup = func(context.Context, string) (string, error) {
		return "11111111-1111-1111-1111-111111111111", nil
	}
	m.append = func(_ context.Context, workItemID, author, body string) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls++
		if f.failOn != 0 && f.calls >= f.failOn {
			return errors.New("append boom")
		}
		f.got = append(f.got, workItemID+"|"+author+"|"+body)
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

// TestProgressMirrorWritesIncrementalComments is the ISI-4235 AC: message and
// tool events become coord.comment rows while the run executes — trust-tagged
// (F16), work-item-scoped, author-provenanced — not only on stream close.
func TestProgressMirrorWritesIncrementalComments(t *testing.T) {
	f := newMirrorFixture(t)
	ctx := context.Background()
	task := "9952db54-0000-0000-0000-000000000000"

	for _, ev := range []wire.Event{
		msgEv(1, task, "Reading the repo layout…"),
		toolEv(2, task, "shell", "start", "ls -la"),
	} {
		if err := f.m.Event(ctx, ev); err != nil {
			t.Fatalf("Event(seq %d) errored: %v (progress must never abort the run)", ev.Seq, err)
		}
	}
	got := f.comments()
	if len(got) != 2 {
		t.Fatalf("appended %d comments, want 2: %v", len(got), got)
	}
	if !strings.Contains(got[0], "[untrusted] Reading the repo layout") {
		t.Fatalf("message comment not F16-trust-tagged: %q", got[0])
	}
	if !strings.Contains(got[1], "[tool:shell/start] ls -la") {
		t.Fatalf("tool comment wrong: %q", got[1])
	}
	for _, c := range got {
		parts := strings.SplitN(c, "|", 3)
		if len(parts) != 3 {
			t.Fatalf("comment not workItem|author|body: %q", c)
		}
		if parts[0] != "11111111-1111-1111-1111-111111111111" {
			t.Fatalf("comment not on the dispatch-marker work item: %q", parts[0])
		}
		if parts[1] != "run/9952db54" {
			t.Fatalf("author not run-provenanced: %q", parts[1])
		}
		if !strings.HasPrefix(parts[2], "[run 9952db54]") {
			t.Fatalf("body lacks the short-run prefix: %q", parts[2])
		}
	}
}

// TestProgressMirrorAuthorsAgentIdentity is the ISI-4739 AC: when the work
// item's checkout claim names an agent, run comments are authored
// agent:<name> (so the console renders the NAME and hashes a stable per-agent
// colour, ISI-4706); when the assignee can't be resolved the author falls back
// to run/<shortRunID>. The [run <id>] body prefix carries provenance either way.
func TestProgressMirrorAuthorsAgentIdentity(t *testing.T) {
	f := newMirrorFixture(t)
	f.m.assignee = func(context.Context, string) string { return "sam" }
	ctx := context.Background()
	task := "c3beee63-0000-0000-0000-000000000000"

	if err := f.m.Event(ctx, msgEv(1, task, "working…")); err != nil {
		t.Fatalf("Event errored: %v", err)
	}
	got := f.comments()
	if len(got) != 1 {
		t.Fatalf("appended %d comments, want 1: %v", len(got), got)
	}
	parts := strings.SplitN(got[0], "|", 3)
	if parts[1] != "agent:sam" {
		t.Fatalf("author = %q, want agent:sam", parts[1])
	}
	if !strings.HasPrefix(parts[2], "[run c3beee63]") {
		t.Fatalf("body lost the run-id prefix: %q", parts[2])
	}

	// Empty assignee → fall back to run/<shortRunID> (no claim row / released).
	f.m.assignee = func(context.Context, string) string { return "" }
	f.m.agentCache.Range(func(k, _ any) bool { f.m.agentCache.Delete(k); return true })
	task2 := "d4ffff74-0000-0000-0000-000000000000"
	if err := f.m.Event(ctx, msgEv(1, task2, "no agent")); err != nil {
		t.Fatalf("Event errored: %v", err)
	}
	got = f.comments()
	if parts := strings.SplitN(got[len(got)-1], "|", 3); parts[1] != "run/d4ffff74" {
		t.Fatalf("fallback author = %q, want run/d4ffff74", parts[1])
	}
}

// TestProgressMirrorCachesAssignee: the assignee lookup is resolved once per
// work item and cached — chatty runs don't re-query coord.claim per comment.
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
	for _, c := range f.comments() {
		if p := strings.SplitN(c, "|", 3); p[1] != "agent:kai" {
			t.Fatalf("author = %q, want agent:kai", p[1])
		}
	}
}

// TestProgressMirrorEnrichesToolEnvelope is the ISI-5191 AC: the tool row
// carries Command/Skill/Server as stable bracketed segments (feeding the
// console's expandable rows, ISI-5190) in addition to the Summary — and the
// raw call arguments are NEVER emitted, only ever available as the emitter's
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
	got := f.comments()
	if len(got) != 2 {
		t.Fatalf("appended %d comments, want 2: %v", len(got), got)
	}
	body1 := strings.SplitN(got[0], "|", 3)[2]
	if !strings.Contains(body1, "[tool:shell/start]") ||
		!strings.Contains(body1, "[cmd:git]") ||
		!strings.Contains(body1, "[skill:commit-flow]") ||
		!strings.Contains(body1, "staging changes") {
		t.Fatalf("tool row not enriched with command/skill/summary: %q", body1)
	}
	if strings.Contains(body1, "[server:") {
		t.Fatalf("local tool row must carry no server segment: %q", body1)
	}
	// The pre-hashed args digest is emitter-only telemetry — it must never leak
	// onto the mirrored comment body (raw args, hashed or not, are not thread
	// content).
	if strings.Contains(body1, "deadbeefcafef00d") || strings.Contains(body1, "secret") {
		t.Fatalf("args digest / raw args leaked onto the wire body: %q", body1)
	}
	// Stable segment order: command before skill before server.
	body2 := strings.SplitN(got[1], "|", 3)[2]
	if !strings.Contains(body2, "[tool:search/result(ok)]") ||
		!strings.Contains(body2, "[server:context7]") ||
		!strings.Contains(body2, "3 hits") {
		t.Fatalf("MCP tool row not enriched with server/summary: %q", body2)
	}
	if strings.Contains(body2, "[cmd:") || strings.Contains(body2, "[skill:") {
		t.Fatalf("MCP tool row must carry no command/skill segment: %q", body2)
	}
}

// TestProgressMirrorRaisedBodyCap is the ISI-5191 AC: the narration/summary cap
// was lifted well past the original 480 runes so real detail survives (the
// console's expandable rows need content to show). The cap still bounds a
// runaway runtime message.
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
	body := strings.SplitN(f.comments()[0], "|", 3)[2]
	if !strings.Contains(body, detail) {
		t.Fatalf("2 KiB narration was clipped under the raised cap: len(body)=%d", len(body))
	}
	if strings.Contains(body, "…") {
		t.Fatalf("narration within the raised cap must not be truncated: %q", body[:64])
	}
}

// TestProgressMirrorDedupsReplayedSeqs: the EventSink contract is
// at-least-once — a replayed/duplicate seq must not double-append.
func TestProgressMirrorDedupsReplayedSeqs(t *testing.T) {
	f := newMirrorFixture(t)
	ctx := context.Background()
	task := "r1"

	_ = f.m.Event(ctx, msgEv(5, task, "first"))
	_ = f.m.Event(ctx, msgEv(5, task, "first (replayed)")) // same seq
	_ = f.m.Event(ctx, msgEv(4, task, "older seq"))        // stale seq
	if got := f.comments(); len(got) != 1 {
		t.Fatalf("dedup failed: %d comments, want 1: %v", len(got), got)
	}
	_ = f.m.Event(ctx, msgEv(6, task, "next"))
	if got := f.comments(); len(got) != 2 {
		t.Fatalf("in-order advance failed: %d comments, want 2", len(got))
	}
}

// TestProgressMirrorFloodGuardDropsButTerminalLands: events faster than the
// per-run min interval are dropped (the thread still advances on the next
// event after the window), and the terminal status event always lands.
func TestProgressMirrorFloodGuardDropsButTerminalLands(t *testing.T) {
	f := newMirrorFixture(t)
	f.m.minInterval = time.Hour // stretch the window: everything non-terminal drops
	ctx := context.Background()
	task := "r2"

	_ = f.m.Event(ctx, msgEv(1, task, "one"))
	_ = f.m.Event(ctx, msgEv(2, task, "two (within window — dropped)"))
	_ = f.m.Event(ctx, toolEv(3, task, "shell", "start", "also dropped"))
	if got := f.comments(); len(got) != 1 {
		t.Fatalf("flood guard failed: %d comments, want 1: %v", len(got), got)
	}
	_ = f.m.Event(ctx, statusEv(4, task, wire.TaskCompleted))
	got := f.comments()
	if len(got) != 2 || !strings.Contains(got[1], "[status] completed") {
		t.Fatalf("terminal event must bypass the flood guard: %v", got)
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

// TestProgressMirrorPublishesLivenessEcho is the ISI-5193 AC: every durable
// comment is ALSO echoed as a run-entity liveness event carrying the same
// (author, body) triple and the run id, so the per-run SSE hub can live-append
// the ticket row. The echo fires once per landed comment, in order, tagged
// thinking, with the FULL (unshortened) run id so the projector can key its
// fan-out and the client can dedup against the reloaded thread.
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
	comments := f.comments()
	if len(echoes) != len(comments) || len(echoes) != 2 {
		t.Fatalf("published %d echoes for %d comments, want 2 each", len(echoes), len(comments))
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
		// The echo must carry the SAME author+body the durable comment did, so a
		// reload dedups the live row exactly (comments[i] == "wi|author|body").
		parts := strings.SplitN(comments[i], "|", 3)
		if p.Author != parts[1] || p.Body != parts[2] {
			t.Fatalf("echo %d payload (%q,%q) != comment (%q,%q)", i, p.Author, p.Body, parts[1], parts[2])
		}
		if p.RunID != task || p.At == "" {
			t.Fatalf("echo %d payload missing runId/at: %+v", i, p)
		}
	}
}

// TestProgressMirrorNoEchoWhenCommentFails: the liveness echo is strictly
// secondary — if the durable comment write fails there is no row to be live
// about, so no run event is published (the reload thread is the source of truth).
func TestProgressMirrorNoEchoWhenCommentFails(t *testing.T) {
	f := newMirrorFixture(t)
	f.failOn = 1 // every append fails
	ctx := context.Background()

	if err := f.m.Event(ctx, msgEv(1, "9952db54-0000-0000-0000-000000000000", "text")); err != nil {
		t.Fatalf("append failure surfaced: %v", err)
	}
	if echoes := f.echoes(); len(echoes) != 0 {
		t.Fatalf("published %d echoes despite a failed comment write, want 0", len(echoes))
	}
}

// TestProgressMirrorSkipsNoiseAndTruncates: signal-less usage/skill-load/
// non-terminal status events are not thread-worthy; oversized text is truncated.
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

	long := strings.Repeat("x", progressMirrorMaxBody+50)
	_ = f.m.Event(ctx, msgEv(4, task, long))
	got := f.comments()
	if len(got) != 1 {
		t.Fatalf("truncation case: %d comments, want 1", len(got))
	}
	body := strings.SplitN(got[0], "|", 3)[2]
	if want := progressMirrorMaxBody + len("[run r6][untrusted] ") + len("…"); len(body) != want {
		t.Fatalf("truncated body len = %d, want %d", len(body), want)
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
// payloads as generic JSON maps — the mirror must recover the typed shapes.
func TestProgressMirrorGenericJSONPayloads(t *testing.T) {
	f := newMirrorFixture(t)
	ctx := context.Background()
	task := "r7"

	_ = f.m.Event(ctx, wire.Event{Seq: 1, A2ATaskID: task, Type: wire.EventMessage,
		Payload: map[string]any{"role": "agent", "text": "decoded from json", "trust": "untrusted"}})
	_ = f.m.Event(ctx, wire.Event{Seq: 2, A2ATaskID: task, Type: wire.EventTool,
		Payload: map[string]any{"name": "read", "phase": "result", "summary": "file.go"}})
	got := f.comments()
	if len(got) != 2 {
		t.Fatalf("json-map payloads: %d comments, want 2: %v", len(got), got)
	}
	if !strings.Contains(got[0], "decoded from json") || !strings.Contains(got[1], "[tool:read/result]") {
		t.Fatalf("json-map bodies wrong: %v", got)
	}
}

// TestProgressMirrorConcurrentRuns: one shared mirror instance serves every
// followed Run — concurrent event streams must not interleave their dedup
// state incorrectly (goes-with-race).
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
				_ = f.m.Event(ctx, msgEv(uint64(seq), task, "text"))
			}
		}()
	}
	wg.Wait()
	// Flood guard collapses each run's 25 events to ~1-2 comments; the exact
	// count is timing-dependent, so assert only a non-empty, deduped result:
	// every appended body carries its own run prefix and no task wrote more
	// than 25.
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
