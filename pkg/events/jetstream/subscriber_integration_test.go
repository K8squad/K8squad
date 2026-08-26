//go:build integration

// End-to-end proof of the plugin subscribe SDK (Story 12.2 / ISI-2914) against a
// REAL NATS/JetStream bus. Run in CI with NATS_URL set:
//
//	go test -tags=integration ./pkg/events/jetstream/ -run TestSubscriber
//
// It lives in package jetstream_test (external) so it can import BOTH pkg/events
// and pkg/events/jetstream, exactly as the relay integration proof does.
package jetstream_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/K8squad/K8squad/pkg/events"
	"github.com/K8squad/K8squad/pkg/events/jetstream"
)

// 12.2 end-to-end: the relay publishes a committed event to the composed
// taxonomy subject, and a plugin using the subscribe SDK receives it, recovers
// the taxonomy from the SUBJECT (not the body, §17.4), and acks it — proving the
// durable consumer replays the already-published message (JetStream catch-up for
// events produced while the plugin was "offline").
func TestSubscriberReceivesPublishedEvent(t *testing.T) {
	url := os.Getenv("NATS_URL")
	if url == "" {
		t.Skip("NATS_URL unset — skipping JetStream subscribe integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Publish one event through the relay half first — it is on the stream BEFORE
	// the subscriber exists, so DeliverAll must replay it (offline catch-up).
	pub, err := jetstream.Connect(ctx, jetstream.Config{URL: url})
	if err != nil {
		t.Fatalf("connect publisher: %v", err)
	}
	defer func() { _ = pub.Close() }()

	store := &memStore{
		rows: []*events.OutboxRow{{ID: 1, Entity: "run", ProjectID: "proj-1", Squad: "", EventType: "completed", Payload: []byte(`{"v":1}`)}},
		pub:  map[int64]bool{},
	}
	relay, err := events.NewRelay(events.RelayConfig{Store: store, Publisher: pub})
	if err != nil {
		t.Fatalf("new relay: %v", err)
	}
	if published, failed, err := relay.Flush(ctx); err != nil || published != 1 || failed != 0 {
		t.Fatalf("flush: pub=%d fail=%d err=%v", published, failed, err)
	}

	// Now subscribe as a plugin would — durable, filtered to run-completed events.
	sub, err := jetstream.Subscribe(ctx, jetstream.SubscribeConfig{
		URL:      url,
		Durable:  "test-plugin-run-completed",
		Subjects: []string{"ksquad.run.*.*.completed"},
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer sub.Close()

	got := make(chan jetstream.Message, 1)
	consumeCtx, consumeCancel := context.WithCancel(ctx)
	defer consumeCancel()
	go func() {
		_ = sub.Consume(consumeCtx, func(_ context.Context, m jetstream.Message) error {
			got <- m
			return nil
		})
	}()

	select {
	case m := <-got:
		if m.Entity != "run" || m.ProjectID != "proj-1" || m.EventType != "completed" {
			t.Fatalf("delivered taxonomy = %+v, want entity=run project=proj-1 event=completed", m.SubjectParts)
		}
		if m.Squad != "" {
			t.Fatalf("Squad = %q, want \"\" (NULL squad token decoded)", m.Squad)
		}
		if string(m.Payload) != `{"v":1}` {
			t.Fatalf("Payload = %q, want the published body verbatim", m.Payload)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no message received via subscribe SDK within 10s (catch-up replay failed)")
	}
}
