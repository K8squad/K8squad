package apiserver

import (
	"context"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/K8squad/K8squad/pkg/events"
)

// ============================================================================
// Run-event → SSE hub projection (§4.4 publish source, ISI-2756)
// ============================================================================
//
// The Hub (sse.go) is transport only — it fans an Event to subscribers but never
// produces one. This file is the production side: it reads run-entity rows off
// coord.outbox (the durable domain-event journal the relay also publishes to
// NATS) and turns them into Hub publishes and Last-Event-ID replays.
//
// Reading the outbox directly — rather than subscribing to NATS — keeps the
// console transport free of a NATS client and makes the outbox the SINGLE source
// of truth for both the live tail and reconnect replay, so their overlap dedups
// exactly by row id. It stays a read-only downstream projection (§17.4): nothing
// here writes the outbox or re-enters coordination, and it is never on a write
// path or the readiness probe — a lagging or failed projection delays console
// progress, it never blocks a Run.

// defaultProjectorPoll is the run-event tail scan cadence. run lifecycle events
// are low-rate relative to this, so a short poll keeps console latency sub-second
// without a LISTEN/NOTIFY dependency (the relay already owns that for NATS).
const defaultProjectorPoll = 1 * time.Second

// ProjectSlugResolver maps a run event's owning Project CR UID (coord.outbox
// project_id — a uuid) to the console's canonical "namespace/name" slug, the key
// the per-project SSE bus and its RBAC gate use (ISI-5194). ok=false ⇒ the UID
// resolves to no live Project (renamed/deleted, or a stale cache) — the projector
// then skips the project bridge for that row, having already fanned it to the
// per-run hub. Best-effort by construction: a missed bridge loses only the live
// project-scoped echo, never the run.
type ProjectSlugResolver func(ctx context.Context, projectUID string) (slug string, ok bool)

// projectBridgeEventTypes is the allowlist of run-event types the projector ALSO
// fans onto the per-project bus. Only `thinking` (ISI-5193) is bridged today —
// the discussion Room (ISI-5208) renders an active run's live thinking deltas
// inline, correlated to its working watch by author. Per-run lifecycle/step
// events stay on the per-run hub (the ticket surface, ISI-5206) and are not
// re-fanned project-wide, keeping the project bus scoped to what a project
// surface consumes.
var projectBridgeEventTypes = map[string]struct{}{
	"thinking": {},
}

// RunEventSource tails coord.outbox for run-entity events and fans each to the
// SSE Hub keyed by run_id — the publish half of §4.4. It is best-effort and
// decoupled: a read error is logged and retried on the next tick, never fatal.
//
// When a ProjectHub + ProjectSlugResolver are wired (WithProjectBridge), it ALSO
// fans the project-scoped subset (projectBridgeEventTypes) onto the per-project
// bus keyed by the run's Project slug, so the discussion Room live-tails an
// active run's thinking without knowing the run id (ISI-5208). The UID→slug
// resolution is memoized for the source's lifetime — a Project's UID→slug binding
// is effectively immutable (a rename mints a new object), so a stale entry can
// only ever mis-route a since-renamed project's echo, which is harmless.
type RunEventSource struct {
	reader     events.RunEventReader
	hub        *Hub
	projectHub *ProjectHub
	resolve    ProjectSlugResolver
	poll       time.Duration
	batch      int
	log        *slog.Logger

	lastID int64 // high-water mark: the last outbox id fanned to the hub

	slugMu    sync.Mutex
	slugCache map[string]string // project UID → "namespace/name" slug (memoized)
}

// RunEventSourceOption tunes a RunEventSource; the zero-config NewRunEventSource
// uses production defaults.
type RunEventSourceOption func(*RunEventSource)

// WithProjectorPoll overrides the tail scan cadence (mainly for tests).
func WithProjectorPoll(d time.Duration) RunEventSourceOption {
	return func(s *RunEventSource) {
		if d > 0 {
			s.poll = d
		}
	}
}

// WithProjectorLogger sets the logger for non-fatal projection errors.
func WithProjectorLogger(l *slog.Logger) RunEventSourceOption {
	return func(s *RunEventSource) {
		if l != nil {
			s.log = l
		}
	}
}

// WithProjectBridge wires the per-project SSE fan-out (ISI-5194) into the
// projector: run events in projectBridgeEventTypes are ALSO published onto hub
// keyed by the run's Project slug, resolved from its outbox project_id UID via
// resolve. Both must be non-nil to arm the bridge; either nil leaves the
// projector fanning to the per-run hub only (the pre-ISI-5208 behavior).
func WithProjectBridge(hub *ProjectHub, resolve ProjectSlugResolver) RunEventSourceOption {
	return func(s *RunEventSource) {
		if hub != nil && resolve != nil {
			s.projectHub = hub
			s.resolve = resolve
		}
	}
}

// NewRunEventSource builds the projector over reader, fanning to hub.
func NewRunEventSource(reader events.RunEventReader, hub *Hub, opts ...RunEventSourceOption) *RunEventSource {
	s := &RunEventSource{
		reader:    reader,
		hub:       hub,
		poll:      defaultProjectorPoll,
		batch:     0, // 0 ⇒ store's default cap per scan
		log:       slog.Default(),
		slugCache: make(map[string]string),
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Run drives the projection until ctx is cancelled. It seeds the watermark at the
// current tail so it fans only NEW events forward — history is served on demand by
// Last-Event-ID replay, not re-fanned to zero subscribers on every restart — then
// drains the run-event tail on each poll tick. It only returns when ctx is done.
func (s *RunEventSource) Run(ctx context.Context) error {
	if latest, err := s.reader.LatestRunEventID(ctx); err != nil {
		// Non-fatal: start the watermark at 0. Worst case we fan the existing backlog
		// once to whoever is subscribed (harmless — Publish to no subscriber is a no-op).
		s.log.Warn("run-event projector: seed watermark failed, starting at 0", "err", err)
	} else {
		s.lastID = latest
	}

	ticker := time.NewTicker(s.poll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			s.drain(ctx)
		}
	}
}

// drain fans every run-event with id > lastID to the hub, in id order, advancing
// the watermark past each. It keeps scanning while a full batch comes back so a
// backlog catches up within one tick instead of one batch per poll; a scan error
// is logged and left for the next tick (the watermark does not advance, so nothing
// is skipped).
func (s *RunEventSource) drain(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		rows, err := s.reader.RunEventsAfter(ctx, s.lastID, s.batch)
		if err != nil {
			if ctx.Err() == nil {
				s.log.Warn("run-event projector: tail scan failed (will retry)", "afterID", s.lastID, "err", err)
			}
			return
		}
		for _, row := range rows {
			sse := runEventToSSE(row)
			s.hub.Publish(row.RunID, sse)
			s.bridgeToProject(ctx, row, sse)
			if row.ID > s.lastID {
				s.lastID = row.ID
			}
		}
		// A short read means the tail is drained; wait for the next tick.
		if s.batch <= 0 || len(rows) < s.batch {
			return
		}
	}
}

// runEventToSSE maps a durable outbox run-event to its SSE wire form: the row id
// (stringified) is the Event.ID / Last-Event-ID resume key, event_type the SSE
// event name, and the jsonb payload the data body.
func runEventToSSE(row events.RunEvent) Event {
	return Event{
		ID:   strconv.FormatInt(row.ID, 10),
		Type: row.EventType,
		Data: string(row.Payload),
	}
}

// bridgeToProject ALSO fans a project-scoped run event (projectBridgeEventTypes)
// onto the per-project bus keyed by the run's Project slug, so a project surface
// (the discussion Room, ISI-5208) live-tails an active run's thinking without
// knowing the run id. A no-op when the bridge is unarmed, the event type is not
// bridged, or the row carries no project_id / resolves to no live Project. The
// SSE event is reused as-is (same id/type/data as the per-run frame) so a project
// subscriber parses it exactly like the per-run stream.
func (s *RunEventSource) bridgeToProject(ctx context.Context, row events.RunEvent, sse Event) {
	if s.projectHub == nil || s.resolve == nil {
		return
	}
	if _, ok := projectBridgeEventTypes[row.EventType]; !ok {
		return
	}
	if row.ProjectID == "" {
		return
	}
	slug, ok := s.projectSlug(ctx, row.ProjectID)
	if !ok {
		return
	}
	s.projectHub.Publish(slug, sse)
}

// projectSlug resolves a Project CR UID to its "namespace/name" slug, memoizing
// the binding for the source's lifetime (a UID→slug binding is effectively
// immutable — a rename mints a new object). A resolver miss is NOT cached, so a
// row that raced the informer cache resolves on a later event.
func (s *RunEventSource) projectSlug(ctx context.Context, uid string) (string, bool) {
	s.slugMu.Lock()
	if slug, ok := s.slugCache[uid]; ok {
		s.slugMu.Unlock()
		return slug, true
	}
	s.slugMu.Unlock()

	slug, ok := s.resolve(ctx, uid)
	if !ok {
		return "", false
	}
	s.slugMu.Lock()
	s.slugCache[uid] = slug
	s.slugMu.Unlock()
	return slug, true
}

// runReplayAdapter adapts events.RunEventReader to the Hub's runReplayer seam,
// converting each per-run RunEvent to an SSE Event for Last-Event-ID replay.
type runReplayAdapter struct{ reader events.RunEventReader }

// NewRunReplayer wraps reader as the Hub replay source (Hub.SetReplayer).
func NewRunReplayer(reader events.RunEventReader) *runReplayAdapter { //nolint:revive // returns an unexported adapter deliberately; callers use it only as the runReplayer seam
	return &runReplayAdapter{reader: reader}
}

func (a *runReplayAdapter) replayRun(ctx context.Context, runID string, afterID int64) ([]Event, error) {
	rows, err := a.reader.RunEventsForRun(ctx, runID, afterID, 0)
	if err != nil {
		return nil, err
	}
	out := make([]Event, 0, len(rows))
	for _, row := range rows {
		out = append(out, runEventToSSE(row))
	}
	return out, nil
}
