package events

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// ---- in-memory fakes mirroring the falsification bench (event-seam-outbox-check.py) ----

type memRow struct {
	row       OutboxRow
	published bool
}

// fakeStore is an in-memory coord.outbox: append-only rows + a set-once
// published flag. It lets the relay logic be proved with no Postgres, exactly as
// the Python bench models Store.
type fakeStore struct {
	mu       sync.Mutex
	rows     []*memRow
	seq      int64
	failMark bool // simulate MarkPublished failing after a good publish
}

func (s *fakeStore) append(r OutboxRow) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	r.ID = s.seq
	s.rows = append(s.rows, &memRow{row: r})
}

func (s *fakeStore) Unpublished(_ context.Context, limit int) ([]OutboxRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []OutboxRow
	for _, mr := range s.rows {
		if mr.published {
			continue
		}
		out = append(out, mr.row)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (s *fakeStore) MarkPublished(_ context.Context, id int64) error {
	if s.failMark {
		return errors.New("mark failed")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, mr := range s.rows {
		if mr.row.ID == id {
			mr.published = true // set-once: a second call is a harmless no-op
		}
	}
	return nil
}

func (s *fakeStore) Depth(_ context.Context) (total, unflushed int64, _ error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, mr := range s.rows {
		total++
		if !mr.published {
			unflushed++
		}
	}
	return total, unflushed, nil
}

// fakePublisher models the JetStream bus with an on/off `up` flag (an outage).
type fakePublisher struct {
	mu        sync.Mutex
	up        bool
	delivered []string // subjects, in delivery order
}

func (p *fakePublisher) Publish(_ context.Context, subject string, _ []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.up {
		return errors.New("nats down")
	}
	p.delivered = append(p.delivered, subject)
	return nil
}
func (p *fakePublisher) Close() error { return nil }
func (p *fakePublisher) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.delivered)
}

// lagPublisher additionally reports consumer lag (exercises the LagReporter path).
type lagPublisher struct {
	fakePublisher
	pending int64
}

func (p *lagPublisher) ConsumerLag(context.Context) (int64, error) { return p.pending, nil }

type fakeMetrics struct {
	depthTotal, depthUnflushed, consumerLag int64
	failures                                int
}

func (m *fakeMetrics) SetDepth(total, unflushed int64) {
	m.depthTotal, m.depthUnflushed = total, unflushed
}
func (m *fakeMetrics) IncPublishFailures()    { m.failures++ }
func (m *fakeMetrics) SetConsumerLag(p int64) { m.consumerLag = p }
func (m *fakeMetrics) Signals() []string {
	return []string{"outbox_depth", "unflushed_lag", "publish_failures", "consumer_lag"}
}

func newRelay(t *testing.T, store OutboxStore, pub Publisher, m Metrics) *Relay {
	t.Helper()
	r, err := NewRelay(RelayConfig{Store: store, Publisher: pub, Metrics: m})
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}
	return r
}

func runEvent(entity, event string) OutboxRow {
	return OutboxRow{Entity: entity, ProjectID: "p1", Squad: "s1", EventType: event, Payload: []byte("{}")}
}

// C2: the relay composes the FULL taxonomy subject from the columns and stamps
// published_at so a second flush does NOT redeliver.
func TestC2_SubjectAndSetOnceStamp(t *testing.T) {
	store := &fakeStore{}
	store.append(runEvent("work_item", "state_changed"))
	pub := &fakePublisher{up: true}
	r := newRelay(t, store, pub, &fakeMetrics{})

	if pubd, failed, err := r.Flush(context.Background()); err != nil || pubd != 1 || failed != 0 {
		t.Fatalf("first flush: pub=%d fail=%d err=%v", pubd, failed, err)
	}
	if got := pub.delivered[0]; got != "ksquad.work_item.p1.s1.state_changed" {
		t.Fatalf("subject = %q, want full ksquad.{entity}.{project}.{squad}.{event_type}", got)
	}
	// Second flush: the row is stamped, so nothing redelivers (set-once).
	if pubd, _, _ := r.Flush(context.Background()); pubd != 0 {
		t.Fatalf("second flush republished %d rows; published_at stamp not honored", pubd)
	}
	if pub.count() != 1 {
		t.Fatalf("delivered %d times, want exactly once", pub.count())
	}
}

// C3: NATS down → the row is buffered (never dropped, never stamped); on
// recovery / restart it delivers exactly once. At-least-once even across an outage.
func TestC3_AtLeastOnceAcrossNatsOutage(t *testing.T) {
	store := &fakeStore{}
	store.append(runEvent("run", "written"))
	pub := &fakePublisher{up: false} // outage
	m := &fakeMetrics{}
	r := newRelay(t, store, pub, m)

	// Flush during the outage: nothing delivered, row retained, failure counted.
	pubd, failed, err := r.Flush(context.Background())
	if err != nil || pubd != 0 || failed != 1 {
		t.Fatalf("outage flush: pub=%d fail=%d err=%v", pubd, failed, err)
	}
	if _, unflushed, _ := store.Depth(context.Background()); unflushed != 1 {
		t.Fatalf("row not buffered during outage (unflushed=%d)", unflushed)
	}
	if m.failures != 1 {
		t.Fatalf("publish_failures=%d, want 1", m.failures)
	}

	// Recovery + a fresh Flush (models a relay restart re-scanning the backlog).
	pub.up = true
	if pubd, failed, _ := r.Flush(context.Background()); pubd != 1 || failed != 0 {
		t.Fatalf("recovery flush: pub=%d fail=%d", pubd, failed)
	}
	if pub.count() != 1 {
		t.Fatalf("delivered %d times after recovery, want exactly once", pub.count())
	}
	if _, unflushed, _ := store.Depth(context.Background()); unflushed != 0 {
		t.Fatalf("row still unflushed after successful delivery (unflushed=%d)", unflushed)
	}
}

// A partial failure (some rows up, publish fails mid-batch) does not abort the
// batch: earlier rows deliver, the failed row is retained for the next tick.
func TestFlush_PartialFailureRetainsFailedRow(t *testing.T) {
	store := &fakeStore{}
	store.append(runEvent("run", "a"))
	store.append(runEvent("run", "b"))
	// A publisher that fails the SECOND publish only.
	var n int
	pub := &togglePublisher{fn: func() bool { n++; return n != 2 }}
	r := newRelay(t, store, pub, &fakeMetrics{})

	pubd, failed, _ := r.Flush(context.Background())
	if pubd != 1 || failed != 1 {
		t.Fatalf("partial: pub=%d fail=%d, want 1/1 (batch not aborted)", pubd, failed)
	}
	if _, unflushed, _ := store.Depth(context.Background()); unflushed != 1 {
		t.Fatalf("failed row not retained (unflushed=%d)", unflushed)
	}
}

type togglePublisher struct{ fn func() bool }

func (p *togglePublisher) Publish(context.Context, string, []byte) error {
	if p.fn() {
		return nil
	}
	return errors.New("boom")
}
func (p *togglePublisher) Close() error { return nil }

// A mark-published failure AFTER a good publish must not lose the event: the row
// stays unflushed and redelivers exactly once (subscribers are idempotent).
func TestFlush_MarkFailureRedeliversOnce(t *testing.T) {
	store := &fakeStore{failMark: true}
	store.append(runEvent("memory", "written"))
	pub := &fakePublisher{up: true}
	r := newRelay(t, store, pub, &fakeMetrics{})

	if _, failed, _ := r.Flush(context.Background()); failed != 1 {
		t.Fatalf("mark-fail flush: failed=%d, want 1", failed)
	}
	// Published on the bus once, but not stamped → next flush redelivers once.
	store.failMark = false
	_, _, _ = r.Flush(context.Background())
	if pub.count() != 2 {
		t.Fatalf("delivered %d times, want 2 (one redelivery after mark failure)", pub.count())
	}
}

// C6: all four §17.2 signals are refreshed on every flush, and consumer lag is
// pulled when the Publisher is a LagReporter.
func TestC6_ObservableAllFourSignals(t *testing.T) {
	store := &fakeStore{}
	store.append(runEvent("run", "started"))
	pub := &lagPublisher{fakePublisher: fakePublisher{up: true}, pending: 7}
	m := &fakeMetrics{}
	r := newRelay(t, store, pub, m)

	_, _, _ = r.Flush(context.Background())
	if m.depthTotal != 1 || m.depthUnflushed != 0 {
		t.Fatalf("depth gauges not refreshed: total=%d unflushed=%d", m.depthTotal, m.depthUnflushed)
	}
	if m.consumerLag != 7 {
		t.Fatalf("consumer_lag=%d, want 7 (LagReporter not consulted)", m.consumerLag)
	}
	want := map[string]bool{"outbox_depth": true, "unflushed_lag": true, "publish_failures": true, "consumer_lag": true}
	if len(m.Signals()) != len(want) {
		t.Fatalf("signals=%v, want the four §17.2 signals", m.Signals())
	}
	for _, s := range m.Signals() {
		if !want[s] {
			t.Fatalf("unexpected signal %q", s)
		}
		delete(want, s)
	}
	if len(want) != 0 {
		t.Fatalf("missing §17.2 signals: %v", want)
	}
}

func TestNewRelay_RequiresStoreAndPublisher(t *testing.T) {
	if _, err := NewRelay(RelayConfig{Publisher: &fakePublisher{}}); err == nil {
		t.Fatal("expected error for missing Store")
	}
	if _, err := NewRelay(RelayConfig{Store: &fakeStore{}}); err == nil {
		t.Fatal("expected error for missing Publisher")
	}
}
