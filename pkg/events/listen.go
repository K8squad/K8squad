package events

import (
	"context"
	"fmt"
	"time"

	"github.com/lib/pq"
)

// pgOutboxChannel is the NOTIFY channel the 0003 trigger fires on each committed
// outbox INSERT (`pg_notify('coord_outbox', id)`).
const pgOutboxChannel = "coord_outbox"

// PgWaker is the production Waker: a Postgres LISTEN on coord_outbox that
// converts each NOTIFY into a relay wake. It is latency-only — pq.Listener
// reconnects on its own and coalesces, and any missed notification is caught by
// the relay's poll — so a dropped wake never costs delivery (C3).
type PgWaker struct {
	listener *pq.Listener
	wakes    chan struct{}
	cancel   context.CancelFunc
}

// NewPgWaker opens a dedicated LISTEN connection on dsn and starts fanning
// notifications into a wake channel. The connection is separate from the relay's
// query pool because a LISTEN connection is long-lived and blocking.
func NewPgWaker(dsn string) (*PgWaker, error) {
	wakes := make(chan struct{}, 1)
	listener := pq.NewListener(dsn, 10*time.Second, time.Minute, func(_ pq.ListenerEventType, err error) {
		// Reconnect/error events are informational; the poll fallback covers any
		// gap, so we deliberately do not surface them as fatal.
		_ = err
	})
	if err := listener.Listen(pgOutboxChannel); err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("events.NewPgWaker: LISTEN %s: %w", pgOutboxChannel, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	w := &PgWaker{listener: listener, wakes: wakes, cancel: cancel}
	go w.pump(ctx)
	return w, nil
}

// pump forwards notifications as non-blocking wakes. The buffered channel + the
// default drop means a burst of NOTIFYs collapses to a single pending wake (the
// relay's flush drains the whole backlog anyway), so the relay is never flooded.
func (w *PgWaker) pump(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.listener.Notify:
			select {
			case w.wakes <- struct{}{}:
			default: // a wake is already pending — coalesce
			}
		case <-time.After(90 * time.Second):
			// Nudge the listener to detect a dead connection between NOTIFYs.
			go func() { _ = w.listener.Ping() }()
		}
	}
}

// Wakes implements Waker.
func (w *PgWaker) Wakes() <-chan struct{} { return w.wakes }

// Close stops the pump and releases the LISTEN connection.
func (w *PgWaker) Close() error {
	w.cancel()
	return w.listener.Close()
}

var _ Waker = (*PgWaker)(nil)
