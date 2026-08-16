// Package events is the domain-event seam (Story 12.1 / ISI-2260, Arch
// §6.6/§17.4, ADR-023 "data in Postgres, events on NATS"). It has two halves
// that meet at the coord.outbox table (db/migrations/0003_coord_outbox.sql):
//
//   - CAPTURE (capture.go). Every state change on a coordination/memory/scm
//     write path appends ONE append-only event row to coord.outbox IN THE SAME
//     transaction as the state change. The row and the state commit atomically,
//     so an event exists iff its state change committed — no lost events, no
//     phantom events, no dual-write hole (AC-a / C1). Capture never owns a
//     transaction boundary; the caller's txn does.
//
//   - RELAY (relay.go). A decoupled worker tails the outbox — LISTEN/NOTIFY on
//     `coord_outbox` for latency plus a periodic poll as the durable fallback —
//     and publishes each unflushed row to the JetStream subject
//     `ksquad.{entity}.{project}.{squad}.{event_type}` (composed from the
//     COLUMNS, never by parsing payload), stamping published_at ONLY on a
//     successful publish. A failed publish leaves published_at NULL and is
//     retried on the next tick / after a restart, so delivery is at-least-once
//     EVEN IF NATS IS DOWN (the outbox is the durable retry buffer). The relay
//     runs OUTSIDE the write transaction and is never wired into apiserver
//     readiness: NATS-down delays fan-out, it never blocks a Run/claim/write
//     (AC-b / C2/C3/C5). Four §17.2 signals make it observable (AC-c / C6).
//
// SCOPE (§17.4 no-P2P guard, 12.4 guardrail): the seam is emit-only and
// one-way. There is no claim/lease/fence column on the outbox and nothing a
// subscriber publishes re-enters coordination. Postgres stays the sole source
// of truth (ADR-001); NATS carries only projections of already-committed state.
//
// The concrete NATS/JetStream Publisher lives in the pkg/events/jetstream
// subpackage so this package (and everything that captures events, e.g.
// pkg/coord) builds WITHOUT the nats.go dependency — only the process that runs
// the relay links it.
package events

import "fmt"

// Entities is the closed set of subject-taxonomy entity families the outbox
// admits. It mirrors the CHECK constraint on coord.outbox.entity (0003): a
// typo'd entity would publish to a garbage NATS subject and silently
// mis-deliver, so Capture rejects it in Go too (fail-fast, before the DB round
// trip) rather than surfacing it as an opaque constraint violation. New event
// sources extend BOTH this set and the CHECK via an additive migration
// (versioned event-catalog discipline, §10.2) — never an ad-hoc string.
var Entities = map[string]bool{
	"run":       true,
	"work_item": true,
	"artifact":  true,
	"memory":    true,
	"scm":       true,
}

// Event is one domain event captured atomically with the state change that
// produced it. The four subject components (Entity/ProjectID/Squad/EventType)
// are first-class fields, not payload keys, so the relay composes the NATS
// subject from columns without parsing the body (§17.4).
type Event struct {
	Entity     string // one of Entities; subject component + outbox CHECK
	ProjectID  string // uuid; tenancy predicate (§12.1) + subject component; REQUIRED
	Squad      string // team_id; "" ⇒ stored NULL ⇒ relay uses the "_" token
	EventType  string // e.g. completed|claimed|handoff|paused|artifact_registered
	WorkItemID string // uuid or ""; optional (Run/scm events need not tie to an item)
	RunID      string // uuid or ""; correlation, optional
	Payload    []byte // versioned jsonb body; nil ⇒ stored as "{}"
}

// validate enforces the invariants Capture depends on before it touches the DB.
func (e Event) validate() error {
	if !Entities[e.Entity] {
		return fmt.Errorf("events: invalid entity %q (allowed: run|work_item|artifact|memory|scm)", e.Entity)
	}
	if e.ProjectID == "" {
		return fmt.Errorf("events: project_id is required (subject taxonomy + tenancy, §12.1)")
	}
	if e.EventType == "" {
		return fmt.Errorf("events: event_type is required (subject taxonomy, §10.2)")
	}
	return nil
}
