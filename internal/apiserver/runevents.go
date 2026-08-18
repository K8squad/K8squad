package apiserver

import (
	"context"
	"log/slog"
	"strconv"
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

// RunEventSource tails coord.outbox for run-entity events and fans each to the
// SSE Hub keyed by run_id — the publish half of §4.4. It is best-effort and
// decoupled: a read error is logged and retried on the next tick, never fatal.
type RunEventSource struct {
	reader events.RunEventReader
	hub    *Hub
	poll   time.Duration
	batch  int
	log    *slog.Logger

	lastID int64 // high-water mark: the last outbox id fanned to the hub
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

// NewRunEventSource builds the projector over reader, fanning to hub.
func NewRunEventSource(reader events.RunEventReader, hub *Hub, opts ...RunEventSourceOption) *RunEventSource {
	s := &RunEventSource{
		reader: reader,
		hub:    hub,
		poll:   defaultProjectorPoll,
		batch:  0, // 0 ⇒ store's default cap per scan
		log:    slog.Default(),
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
			s.hub.Publish(row.RunID, runEventToSSE(row))
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
