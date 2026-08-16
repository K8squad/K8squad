//go:build integration

// End-to-end proof of the relay half against a REAL NATS/JetStream bus (and,
// when DATABASE_URL is set, a real outbox). Run in CI with NATS_URL set:
//
//	go test -tags=integration ./pkg/events/jetstream/ -run TestRelay
//
// It lives in package jetstream_test (external) so it can import BOTH pkg/events
// and pkg/events/jetstream without the import cycle an internal pkg/events test
// would hit (jetstream imports events).
package jetstream_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/K8squad/K8squad/pkg/events"
	"github.com/K8squad/K8squad/pkg/events/jetstream"
)

// memStore is a tiny in-memory OutboxStore so the NATS proof does not also
// require Postgres — the DB-backed SQLStore is proved separately in
// pkg/events/integration_test.go.
type memStore struct {
	rows []*events.OutboxRow
	pub  map[int64]bool
}

func (m *memStore) Unpublished(_ context.Context, _ int) ([]events.OutboxRow, error) {
	var out []events.OutboxRow
	for _, r := range m.rows {
		if !m.pub[r.ID] {
			out = append(out, *r)
		}
	}
	return out, nil
}
func (m *memStore) MarkPublished(_ context.Context, id int64) error { m.pub[id] = true; return nil }
func (m *memStore) Depth(_ context.Context) (int64, int64, error) {
	var total, unflushed int64
	for _, r := range m.rows {
		total++
		if !m.pub[r.ID] {
			unflushed++
		}
	}
	return total, unflushed, nil
}

// C2 end-to-end: the relay publishes to the composed taxonomy subject, a
// subscriber receives it, and the row is stamped so it does not redeliver.
func TestRelayPublishesToJetStream(t *testing.T) {
	url := os.Getenv("NATS_URL")
	if url == "" {
		t.Skip("NATS_URL unset — skipping JetStream integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pub, err := jetstream.Connect(ctx, jetstream.Config{URL: url})
	if err != nil {
		t.Fatalf("connect jetstream: %v", err)
	}
	defer func() { _ = pub.Close() }()

	// Subscribe (core NATS sub is enough to observe the published subject).
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("nats connect: %v", err)
	}
	defer nc.Close()
	got := make(chan string, 4)
	sub, err := nc.Subscribe("ksquad.>", func(m *nats.Msg) { got <- m.Subject })
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() { _ = sub.Unsubscribe() }()

	store := &memStore{
		rows: []*events.OutboxRow{{ID: 1, Entity: "work_item", ProjectID: "p1", Squad: "s1", EventType: "claimed", Payload: []byte(`{"v":1}`)}},
		pub:  map[int64]bool{},
	}
	relay, err := events.NewRelay(events.RelayConfig{Store: store, Publisher: pub, Metrics: events.NewPrometheusMetrics()})
	if err != nil {
		t.Fatalf("new relay: %v", err)
	}

	published, failed, err := relay.Flush(ctx)
	if err != nil || published != 1 || failed != 0 {
		t.Fatalf("flush: pub=%d fail=%d err=%v", published, failed, err)
	}
	select {
	case subj := <-got:
		if subj != "ksquad.work_item.p1.s1.claimed" {
			t.Fatalf("delivered subject %q, want full taxonomy", subj)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no message delivered within 5s")
	}
	if !store.pub[1] {
		t.Fatal("row not marked published after successful delivery")
	}

	// Set-once: a second flush must not redeliver.
	published, _, _ = relay.Flush(ctx)
	if published != 0 {
		t.Fatalf("second flush republished %d rows, want 0", published)
	}
}
