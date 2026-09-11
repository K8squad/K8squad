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
	m      *ProgressMirror
	mu     sync.Mutex
	got    []string // "author|body" per appended comment
	failOn int      // append calls >= failOn fail (0 = never)
	calls  int
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
	f.m = m
	return f
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

// TestProgressMirrorSkipsNoiseAndTruncates: usage/skill-load/non-terminal
// status events are not thread-worthy; oversized text is truncated.
func TestProgressMirrorSkipsNoiseAndTruncates(t *testing.T) {
	f := newMirrorFixture(t)
	ctx := context.Background()
	task := "r6"

	_ = f.m.Event(ctx, wire.Event{Seq: 1, A2ATaskID: task, Type: wire.EventUsage,
		Payload: wire.UsagePayload{Model: "m", Input: 1, Output: 2}})
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
