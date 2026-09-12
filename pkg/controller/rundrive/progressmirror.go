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

// progressmirror.go — the M1.6 "progress visible (ugly-but-real)" surface
// (ISI-4235). The shim streams typed progress events (message/tool_use) over
// the supervisor's NDJSON / stdio JSONL wire and the operator-side dispatcher
// follows them, but nothing turned those events into coord writes the console
// ticket thread renders: cmd/operator/main.go never set RunEvents, so the
// per-Run TelemetrySink degraded its inner sink to DiscardSink and every
// progress event died there — the demo ticket accumulated zero coord.comment
// rows. ProgressMirror is that missing inner sink: it appends F16-trust-tagged
// coord.comment rows on the Run's work item WHILE the run executes
// (incremental, one INSERT per mirrored event — never batched on stream
// close), resolving the work item through the durable coord.a2a_dispatch
// marker (a2a_task_id → work_item_id; the PK matches the wire task id
// exactly, #lapN retry-lap suffix included).
//
// EventSink contract notes (internal/a2a client.go): delivery is at-least-once
// so the mirror dedups on (a2a_task_id, seq) in memory, and a sink error
// CANCELS the live task — so the mirror NEVER returns one. Its own failures
// (lookup miss, DB hiccup) are dropped: progress mirroring must never kill a
// run. Volume is bounded by a per-run min interval — chatty runtimes drop
// intermediate events instead of flooding the append-only comment channel —
// and terminal status events bypass the guard so the outcome always lands.
// This is deliberately the sanctioned §6.1 write path (coord.AppendComment),
// never a lateral INSERT of its own.
package rundrive

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	a2a "github.com/K8squad/K8squad/internal/a2a"
	wire "github.com/K8squad/K8squad/pkg/a2a"
	"github.com/K8squad/K8squad/pkg/coord"
)

// ProgressMirror satisfies the dispatcher's EventSink seam (RunEvents) — the
// compile-time pin keeps the §5.1 contract (Event(ctx, wire.Event) error)
// honest as the internal/a2a client evolves.
var _ a2a.EventSink = (*ProgressMirror)(nil)

const (
	// progressMirrorMinInterval bounds the per-run comment rate: events
	// arriving faster are DROPPED (not queued — the next event after the
	// window lands, so the thread still advances). Terminal status events
	// bypass the guard.
	progressMirrorMinInterval = time.Second
	// progressMirrorMaxBody caps one mirrored comment's body so a runaway
	// runtime message cannot balloon the append-only channel row.
	progressMirrorMaxBody = 480
	// progressMirrorMaxTracked caps the dedup/rate maps: past it they reset
	// (worst case one duplicate comment on a live replay — acceptable, and
	// far better than unbounded growth over the operator's lifetime).
	progressMirrorMaxTracked = 4096
)

// ProgressMirror is the operator-side a2a.EventSink that mirrors shim progress
// events into coord.comment rows on the Run's work item (M1.6 AC: incremental,
// trust-tagged, ugly-but-real). It is safe for concurrent use — one instance
// is shared by every followed Run through OperatorDispatchConfig.RunEvents.
type ProgressMirror struct {
	db *sql.DB

	// lookup resolves an a2a_task_id to its work item id (default: the
	// coord.a2a_dispatch marker over db). Test seam.
	lookup func(ctx context.Context, a2aTaskID string) (string, error)
	// append appends one comment (default: coord.AppendComment over db).
	// Test seam.
	append func(ctx context.Context, workItemID, author, body string) error

	// minInterval bounds the per-run comment rate: events arriving faster
	// are DROPPED (not queued — the next event after the window lands, so
	// the thread still advances). Terminal status events bypass the guard.
	// Field (not const) so tests can zero or stretch it.
	minInterval time.Duration

	mu      sync.Mutex
	lastSeq map[string]uint64    // a2aTaskID → last mirrored seq (at-least-once dedup, C4)
	lastAt  map[string]time.Time // runID → last comment time (flood guard)

	wiCache sync.Map // a2aTaskID → workItemID (resolved lookups only)
}

// NewProgressMirror returns the mirror bound to the coordination Postgres.
func NewProgressMirror(db *sql.DB) *ProgressMirror {
	m := &ProgressMirror{
		db:          db,
		minInterval: progressMirrorMinInterval,
		lastSeq:     make(map[string]uint64),
		lastAt:      make(map[string]time.Time),
	}
	m.lookup = m.lookupDispatchMarker
	m.append = func(ctx context.Context, workItemID, author, body string) error {
		_, err := coord.AppendComment(ctx, m.db, workItemID, author, body)
		return err
	}
	return m
}

// Event implements a2a.EventSink. It never returns a non-nil error: per the
// §5.1 sink contract an error aborts (cancels) the live task, and progress
// mirroring is strictly best-effort — its failures are silently dropped so the
// Run's execution and settlement path are untouched.
func (m *ProgressMirror) Event(ctx context.Context, ev wire.Event) error {
	if m == nil || m.db == nil {
		return nil
	}
	runID := cleanRunID(ev.A2ATaskID)
	body := mirrorBody(runID, ev)
	if body == "" {
		return nil // not a progress surface (usage/skill-load/…, or empty text)
	}

	m.mu.Lock()
	if ev.Seq <= m.lastSeq[ev.A2ATaskID] {
		m.mu.Unlock()
		return nil // duplicate / replayed (at-least-once redelivery)
	}
	if len(m.lastSeq) >= progressMirrorMaxTracked {
		m.lastSeq = make(map[string]uint64)
		m.lastAt = make(map[string]time.Time)
	}
	m.lastSeq[ev.A2ATaskID] = ev.Seq
	if !isTerminalEvent(ev) {
		if last, ok := m.lastAt[runID]; ok && time.Since(last) < m.minInterval {
			m.mu.Unlock()
			return nil // flood guard: drop, never queue
		}
	}
	m.lastAt[runID] = time.Now()
	m.mu.Unlock()

	wi, err := m.workItem(ctx, ev.A2ATaskID)
	if err != nil || wi == "" {
		return nil // marker not written yet, or transient DB failure — drop
	}
	_ = m.append(ctx, wi, "run/"+shortRunID(runID), body)
	return nil
}

// workItem resolves the a2a_task_id through the durable dispatch marker,
// caching resolved ids for the operator's lifetime of the task.
func (m *ProgressMirror) workItem(ctx context.Context, a2aTaskID string) (string, error) {
	if v, ok := m.wiCache.Load(a2aTaskID); ok {
		return v.(string), nil
	}
	wi, err := m.lookup(ctx, a2aTaskID)
	if err != nil || wi == "" {
		return "", err // do not cache failures — the next event retries
	}
	m.wiCache.Store(a2aTaskID, wi)
	return wi, nil
}

// lookupDispatchMarker is the production lookup: the §6.4 idempotency marker's
// PK is the deterministic a2a_task_id (run uuid, or run uuid#lapN), so this is
// an exact one-row read that stays valid for the whole Run — unlike
// coord.claim, which the settle path rewrites/releases.
func (m *ProgressMirror) lookupDispatchMarker(ctx context.Context, a2aTaskID string) (string, error) {
	var wi sql.NullString
	err := m.db.QueryRowContext(ctx,
		`SELECT work_item_id::text FROM coord.a2a_dispatch WHERE a2a_task_id = $1`, a2aTaskID).
		Scan(&wi)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil // not an error: event raced the marker write — drop, retry next event
	}
	if err != nil {
		return "", fmt.Errorf("rundrive.ProgressMirror: lookup dispatch marker: %w", err)
	}
	return wi.String, nil
}

// isTerminalEvent reports whether ev is the task's terminal status event (the
// one progress signal that must never be dropped by the flood guard).
func isTerminalEvent(ev wire.Event) bool {
	if ev.Type != wire.EventStatus {
		return false
	}
	p, ok := mirrorStatusPayload(ev.Payload)
	return ok && p.State.IsTerminal()
}

// mirrorBody renders one event's F16-trust-tagged comment body ("" = skip).
// Agent text and tool summaries are runtime-authored — always untrusted,
// displayed never executed (F16); the short run id prefixes each row so a
// multi-run thread stays readable.
func mirrorBody(runID string, ev wire.Event) string {
	tag := "[run " + shortRunID(runID) + "]"
	switch ev.Type {
	case wire.EventMessage:
		p, ok := mirrorMessagePayload(ev.Payload)
		if !ok || p.Text == "" {
			return ""
		}
		return tag + "[untrusted] " + truncateRun(p.Text)
	case wire.EventTool:
		p, ok := mirrorToolPayload(ev.Payload)
		if !ok || p.Name == "" {
			return ""
		}
		phase := p.Phase
		if phase == "result" && p.OK != nil {
			if *p.OK {
				phase = "result(ok)"
			} else {
				phase = "result(err)"
			}
		}
		body := tag + "[tool:" + p.Name + "/" + phase + "]"
		if p.Summary != "" {
			body += " " + truncateRun(p.Summary)
		}
		return body
	case wire.EventStatus:
		p, ok := mirrorStatusPayload(ev.Payload)
		if !ok || !p.State.IsTerminal() {
			return "" // non-terminal status churn is not thread-worthy
		}
		body := tag + "[status] " + string(p.State)
		if p.Reason != "" {
			body += ": " + truncateRun(p.Reason)
		}
		return body
	}
	return ""
}

// truncateRun caps s at progressMirrorMaxBody runes with an explicit ellipsis.
func truncateRun(s string) string {
	r := []rune(s)
	if len(r) <= progressMirrorMaxBody {
		return s
	}
	return string(r[:progressMirrorMaxBody]) + "…"
}

// shortRunID renders the display form of a run uid for comment prefixes.
func shortRunID(runID string) string {
	if len(runID) <= 8 {
		return runID
	}
	return runID[:8]
}

// mirrorMessagePayload / mirrorToolPayload / mirrorStatusPayload normalize a
// wire payload into its typed shape, tolerating both the in-process concrete
// value and the generic JSON map the stdio/NDJSON transports decode (same
// discipline as internal/a2a's normalizers, kept local so this sink adds no
// export surface to the client package).
func mirrorMessagePayload(payload any) (wire.MessagePayload, bool) {
	switch v := payload.(type) {
	case wire.MessagePayload:
		return v, true
	case *wire.MessagePayload:
		if v == nil {
			return wire.MessagePayload{}, false
		}
		return *v, true
	}
	return decodePayload[wire.MessagePayload](payload)
}

func mirrorToolPayload(payload any) (wire.ToolPayload, bool) {
	switch v := payload.(type) {
	case wire.ToolPayload:
		return v, true
	case *wire.ToolPayload:
		if v == nil {
			return wire.ToolPayload{}, false
		}
		return *v, true
	}
	return decodePayload[wire.ToolPayload](payload)
}

func mirrorStatusPayload(payload any) (wire.StatusPayload, bool) {
	switch v := payload.(type) {
	case wire.StatusPayload:
		return v, true
	case *wire.StatusPayload:
		if v == nil {
			return wire.StatusPayload{}, false
		}
		return *v, true
	}
	return decodePayload[wire.StatusPayload](payload)
}

func decodePayload[T any](payload any) (T, bool) {
	var zero T
	b, err := json.Marshal(payload)
	if err != nil {
		return zero, false
	}
	var out T
	if err := json.Unmarshal(b, &out); err != nil {
		return zero, false
	}
	return out, true
}
