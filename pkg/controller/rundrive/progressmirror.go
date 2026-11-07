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
//
// ISI-7099 (ISI-7074 WS-2) splits the mirror into two tiers: ephemeral
// thinking (non-deliverable messages, tool calls) publishes to the live run
// SSE stream ONLY — never a durable coord.comment row — while the WS-1
// deliverable-tagged message becomes the run's ONE kind='summary' comment
// (coord.AppendSummary, counted by settle's exactly-one invariant), immune to
// the flood guard. Usage digests and terminal status rows stay kind='progress'
// Activity rows.
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
	"github.com/K8squad/K8squad/pkg/events"
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
	// progressMirrorMaxBody caps one mirrored comment's variable-length field
	// (narration text, tool summary, status reason) so a runaway runtime
	// message cannot balloon the append-only channel row. Raised from the
	// original 480 (ISI-5191): 480 clipped real narration and tool output to
	// uselessly-terse stubs, so the console's expandable rows (ISI-5190) had
	// nothing to expand. 8 KiB carries genuine detail while still bounding the
	// worst case — the full reasoning/prompt/response TEXT is a separate
	// producer surface gated on the observability spine (ISI-4812), not this
	// best-effort mirror.
	progressMirrorMaxBody = 8192
	// progressMirrorMaxTracked caps the dedup/rate maps: past it they reset
	// (worst case one duplicate comment on a live replay — acceptable, and
	// far better than unbounded growth over the operator's lifetime).
	progressMirrorMaxTracked = 4096
	// progressMirrorThinkingInterval paces the EPHEMERAL thinking tier
	// (ISI-7099 / ISI-7074 WS-2): SSE-only rows are cheap (one outbox event,
	// no durable coord.comment), so the plan's "relax rather than drop"
	// applies — 4x looser than the durable-row guard so the live trace stays
	// rich, while still bounding per-run outbox churn. The deliverable and
	// terminal statuses bypass every guard.
	progressMirrorThinkingInterval = 250 * time.Millisecond
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
	// assignee resolves a work item id to the agent name that owns its
	// checkout claim (coord.claim.assignee_agent — default over db). "" when
	// unset/unresolvable. Test seam.
	assignee func(ctx context.Context, workItemID string) string
	// append appends one PROGRESS comment (default: coord.AppendProgress over db,
	// ISI-7094 — tagged kind='progress' with the run id so the thread excludes it
	// and the console groups it under the per-run Activity expander). Test seam.
	append func(ctx context.Context, workItemID, author, runID, body string) error
	// publish emits the liveness projection of a mirrored comment as a
	// run-entity outbox event (default: events.CaptureRunForWorkItem over db),
	// which the apiserver's run-event projector fans onto the EXISTING per-run
	// SSE hub so the ticket Activity feed goes live without a second EventSource
	// (ISI-5193, plan ISI-5185/WS-A). Best-effort and strictly secondary to the
	// durable coord.comment write — a publish failure loses only the live
	// signal; the reload thread still carries the row. Test seam.
	publish func(ctx context.Context, workItemID, runID, eventType string, payload []byte) error

	// appendSummary appends the run's ONE deliverable comment (default:
	// coord.AppendSummary over db — ISI-7099 / ISI-7074 WS-2, kind='summary',
	// run-id stamped so settle's exactly-one invariant counts it). Test seam.
	appendSummary func(ctx context.Context, workItemID, author, runID, body string) error

	// minInterval bounds the per-run DURABLE row rate (progress-tier rows):
	// events arriving faster are DROPPED (not queued — the next event after
	// the window lands, so the thread still advances). Terminal status events
	// bypass the guard, and the deliverable tier is never guarded at all.
	// Field (not const) so tests can zero or stretch it.
	minInterval time.Duration

	// pubInterval bounds the per-run EPHEMERAL thinking-publish rate: the
	// outbox event is still a DB write, so chatty narration stays paced, but
	// looser than minInterval because a dropped live row costs only liveness,
	// never the thread. Field (not const) so tests can zero or stretch it.
	pubInterval time.Duration

	mu        sync.Mutex
	lastSeq   map[string]uint64    // a2aTaskID → last mirrored seq (at-least-once dedup, C4)
	lastAt    map[string]time.Time // runID → last durable row time (flood guard)
	lastPubAt map[string]time.Time // runID → last ephemeral publish (relaxed pacing)
	delivered map[string]bool      // runID → deliverable summary already written (exactly-one)

	wiCache    sync.Map // a2aTaskID → workItemID (resolved lookups only)
	agentCache sync.Map // workItemID → agentName (resolved non-empty only)
}

// NewProgressMirror returns the mirror bound to the coordination Postgres.
func NewProgressMirror(db *sql.DB) *ProgressMirror {
	m := &ProgressMirror{
		db:          db,
		minInterval: progressMirrorMinInterval,
		pubInterval: progressMirrorThinkingInterval,
		lastSeq:     make(map[string]uint64),
		lastAt:      make(map[string]time.Time),
		lastPubAt:   make(map[string]time.Time),
		delivered:   make(map[string]bool),
	}
	m.lookup = m.lookupDispatchMarker
	m.assignee = m.lookupAssigneeAgent
	m.append = func(ctx context.Context, workItemID, author, runID, body string) error {
		_, err := coord.AppendProgress(ctx, m.db, workItemID, author, runID, body)
		return err
	}
	m.appendSummary = func(ctx context.Context, workItemID, author, runID, body string) error {
		_, err := coord.AppendSummary(ctx, m.db, workItemID, author, runID, body)
		return err
	}
	m.publish = func(ctx context.Context, workItemID, runID, eventType string, payload []byte) error {
		return events.CaptureRunForWorkItem(ctx, m.db, workItemID, runID, eventType, payload)
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
	t := mirrorTierOf(ev)
	var body string
	switch t {
	case mirrorTierDeliverable:
		body = deliverableBody(ev)
	default:
		body = mirrorBody(runID, ev)
	}
	if body == "" {
		return nil // not a progress surface (usage/skill-load/…, or empty text)
	}

	reserveDeliverable := false
	m.mu.Lock()
	if ev.Seq <= m.lastSeq[ev.A2ATaskID] {
		m.mu.Unlock()
		return nil // duplicate / replayed (at-least-once redelivery)
	}
	if len(m.lastSeq) >= progressMirrorMaxTracked {
		m.lastSeq = make(map[string]uint64)
		m.lastAt = make(map[string]time.Time)
		m.lastPubAt = make(map[string]time.Time)
		m.delivered = make(map[string]bool)
	}
	m.lastSeq[ev.A2ATaskID] = ev.Seq
	switch {
	case t == mirrorTierDeliverable || (t == mirrorTierProgress && isTerminalEvent(ev)):
		// The deliverable and the terminal status bypass every guard: the
		// run's one comment and its outcome must never be dropped (ISI-7099).
	case t == mirrorTierEphemeral:
		if last, ok := m.lastPubAt[runID]; ok && time.Since(last) < m.pubInterval {
			m.mu.Unlock()
			return nil // ephemeral pacing (relaxed): drop, never queue — liveness is lossy by design
		}
		m.lastPubAt[runID] = time.Now()
	default:
		if last, ok := m.lastAt[runID]; ok && time.Since(last) < m.minInterval {
			m.mu.Unlock()
			return nil // flood guard on durable rows: drop, never queue
		}
		m.lastAt[runID] = time.Now()
	}
	// ISI-7099 exactly-one: the first deliverable stands; a later one is
	// demoted to the ephemeral tier so the thread never gains a second
	// comment. The engine funnel (ISI-7098) already enforces this upstream —
	// this is the mirror-side structural pin, never parser goodwill.
	if t == mirrorTierDeliverable {
		if m.delivered[runID] {
			t = mirrorTierEphemeral
		} else {
			m.delivered[runID] = true
			reserveDeliverable = true
		}
	}
	m.mu.Unlock()

	wi, err := m.workItem(ctx, ev.A2ATaskID)
	if err != nil || wi == "" {
		return nil // marker not written yet, or transient DB failure — drop
	}
	// Author with the agent identity (agent:<name>) so the console can render
	// the agent NAME and hash a stable per-agent colour (ISI-4706); fall back
	// to run/<shortRunID> only when the assignee can't be resolved. The
	// [run <id>] body prefix carries the run id either way, so provenance is
	// never lost.
	author := "run/" + shortRunID(runID)
	if name := m.agentName(ctx, wi); name != "" {
		author = "agent:" + name
	}
	switch t {
	case mirrorTierEphemeral:
		// ISI-7099: thinking/tool narration lives on the live run SSE stream
		// ONLY — never a durable coord.comment row (FR-B3 §6.1: the ephemeral
		// tier routes through the run outbox, not a comment INSERT).
		m.publishThinking(ctx, wi, runID, author, body, ev.Seq)
		return nil
	case mirrorTierDeliverable:
		if m.appendSummary == nil || m.appendSummary(ctx, wi, author, runID, body) != nil {
			if reserveDeliverable {
				m.mu.Lock()
				m.delivered[runID] = false // a later deliverable may retry; settle's synth backstops the recap
				m.mu.Unlock()
			}
			return nil // dropped — settle's synthesized-summary backstop still owes the run exactly one recap
		}
	default:
		if err := m.append(ctx, wi, author, runID, body); err != nil {
			return nil // the durable comment is the source of truth — no comment, no liveness echo
		}
	}
	// Liveness echo (ISI-5193): the row is durable (or the deliverable just
	// landed), so project it as a run-entity outbox event so the per-run SSE hub
	// can live-append it to the ticket. Purely best-effort — a publish miss only
	// delays the row to the next thread reload, never the run.
	m.publishThinking(ctx, wi, runID, author, body, ev.Seq)
	return nil
}

// mirrorTier classifies an event into its ISI-7099 handling tier: the WS-1
// deliverable (a kind=deliverable message) is the run's ONE durable comment;
// usage digests and terminal status stay kind='progress' Activity rows; every
// other progress surface (thinking messages, tool calls) is EPHEMERAL — live
// SSE only, never a durable coord.comment row.
type mirrorTier int

const (
	mirrorTierEphemeral   mirrorTier = iota // thinking/tool narration: live SSE echo only
	mirrorTierProgress                      // usage digests + terminal status: durable Activity row + echo
	mirrorTierDeliverable                   // the WS-1 final answer: the run's one kind='summary' comment
)

// mirrorTierOf classifies ev. An EventMessage tagged kind=deliverable (WS-1,
// ISI-7098) is the deliverable; any other message — kind absent
// (legacy/unclassified) or thinking — and every tool call is ephemeral, per
// the board's default that thinking is live-only (ISI-7074 decision 3).
func mirrorTierOf(ev wire.Event) mirrorTier {
	if ev.Type == wire.EventMessage {
		if p, ok := mirrorMessagePayload(ev.Payload); ok && p.Kind == wire.MessageKindDeliverable {
			return mirrorTierDeliverable
		}
		return mirrorTierEphemeral
	}
	if ev.Type == wire.EventTool {
		return mirrorTierEphemeral
	}
	return mirrorTierProgress
}

// deliverableBody renders the run's ONE durable comment from the WS-1
// deliverable text: clean prose, never the [run/[untrusted] progress tokens —
// a summary is the human-readable recap the board reads (AppendSummary
// contract), and the agent principal already carries F16 provenance.
func deliverableBody(ev wire.Event) string {
	p, ok := mirrorMessagePayload(ev.Payload)
	if !ok {
		return ""
	}
	return truncateRun(p.Text)
}

// thinkingEventType is the SSE event name (outbox event_type) the ticket's
// useRunStream subscribes to for a live mirrored row. Bare word, no CR/LF, so it
// is a valid `event:` line and a valid NATS subject token.
const thinkingEventType = "thinking"

// thinkingPayload is the run-event body carried on the per-run SSE stream. It is
// the SAME triple the durable coord.comment row holds (author_principal, body,
// created_at) plus the run id and wire seq, so the console renders a live row
// identical to the one a subsequent thread reload materializes — the client
// dedups the two by (author, body) when the durable row lands.
type thinkingPayload struct {
	RunID  string `json:"runId"`
	Author string `json:"author"`
	Body   string `json:"body"`
	Seq    uint64 `json:"seq"`
	At     string `json:"at"` // RFC3339Nano, UTC — client-render timestamp until reload
}

// publishThinking emits one mirrored row — durable comment or ephemeral
// thinking — as a run-entity outbox event. All failures (nil seam, marshal
// error, DB hiccup, non-uuid run id) are swallowed: the liveness echo must
// never perturb a run whose durable record already landed.
func (m *ProgressMirror) publishThinking(ctx context.Context, workItemID, runID, author, body string, seq uint64) {
	if m.publish == nil {
		return
	}
	payload, err := json.Marshal(thinkingPayload{
		RunID:  runID,
		Author: author,
		Body:   body,
		Seq:    seq,
		At:     time.Now().UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		return
	}
	_ = m.publish(ctx, workItemID, runID, thinkingEventType, payload)
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

// agentName resolves the agent that owns work item wi, caching non-empty
// results for the operator's lifetime of the task. The checkout claim's
// assignee_agent is stamped at acquire and stable through the run (ISI-4237),
// so one resolution serves every mirrored comment; empty results are never
// cached, so a comment that raced the claim write re-resolves on the next event.
func (m *ProgressMirror) agentName(ctx context.Context, workItemID string) string {
	if v, ok := m.agentCache.Load(workItemID); ok {
		return v.(string)
	}
	if m.assignee == nil {
		return ""
	}
	name := m.assignee(ctx, workItemID)
	if name != "" {
		m.agentCache.Store(workItemID, name)
	}
	return name
}

// lookupAssigneeAgent is the production assignee resolver: it reads the agent
// stamped on the work item's checkout claim (coord.claim.assignee_agent, the
// exact lookup coord.SettleTerminalLane uses — ISI-4237). Empty on any miss
// (no claim row, null assignee, transient DB error): the caller falls back to
// run/<shortRunID>, so a lookup hiccup only downgrades attribution, never the
// mirror.
func (m *ProgressMirror) lookupAssigneeAgent(ctx context.Context, workItemID string) string {
	var agent sql.NullString
	if err := m.db.QueryRowContext(ctx,
		`SELECT assignee_agent FROM coord.claim WHERE work_item_id = $1::uuid`,
		workItemID).Scan(&agent); err != nil {
		return "" // no claim row / released / transient — fall back to run/<id>
	}
	return agent.String
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
		// Enrichment segments (ISI-5191): each is an optional bracketed
		// key:value the console parses into an expandable tool row (ISI-5190).
		// Order is stable (command → skill → server) so the parser splits
		// deterministically, and every value is already a low-cardinality,
		// PII-safe token the shim extracted in-process (ISI-4720): Command is
		// a recognized shell head (git/kubectl/…), Skill/Server are curated
		// identifiers. Raw call arguments are NEVER put on this wire — the
		// emitter hashes them to ToolPayload.ArgsSHA256 before the event
		// leaves the process (Epic D, plan §2.4).
		if p.Command != "" {
			body += "[cmd:" + p.Command + "]"
		}
		if p.Skill != "" {
			body += "[skill:" + p.Skill + "]"
		}
		if p.Server != "" {
			body += "[server:" + p.Server + "]"
		}
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
	case wire.EventUsage:
		p, ok := mirrorUsagePayload(ev.Payload)
		if !ok {
			return ""
		}
		return mirrorUsageBody(tag, p)
	}
	return ""
}

// mirrorUsageBody renders the CR LLMInteraction "response" digest into the
// ticket feed (ISI-5192): the same model + token summary run-detail already
// surfaces from Run.Status.LLMInteractions (activityFromInteraction → the
// "response" case), carried here as a parseable `[llm:response]` envelope so
// the ticket agent-turn card can show that a model round-trip completed — a
// signal the ticket thread otherwise never saw (only run-detail did, off the
// CR). It is deliberately a DIGEST — model + bounded token counts, matching
// what the CR LLMInteraction stores — NEVER the response TEXT (opt-in content
// capture is hard-gated on ISI-4812). "" when the usage event carries no
// signal worth a row (no model and no tokens), so empty churn stays silent.
func mirrorUsageBody(tag string, p wire.UsagePayload) string {
	model := p.Model
	if model == "" {
		model = p.ResponseModel
	}
	total := p.Input + p.Output
	if model == "" && total == 0 {
		return "" // no signal — matches the run-detail "honest empty" posture
	}
	if model == "" {
		model = "model" // token-bearing usage from an empty-model runtime (ISI-4412)
	}
	body := tag + "[llm:response] " + model
	if total > 0 {
		body += fmt.Sprintf(" · %d tok (%d in / %d out)", total, p.Input, p.Output)
	}
	return body
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

func mirrorUsagePayload(payload any) (wire.UsagePayload, bool) {
	switch v := payload.(type) {
	case wire.UsagePayload:
		return v, true
	case *wire.UsagePayload:
		if v == nil {
			return wire.UsagePayload{}, false
		}
		return *v, true
	}
	return decodePayload[wire.UsagePayload](payload)
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
