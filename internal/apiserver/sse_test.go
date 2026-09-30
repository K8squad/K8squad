package apiserver

import (
	"bufio"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"

	"github.com/K8squad/K8squad/pkg/events"
)

// streamHarness mounts hub.streamRun on a mux (so {runId} resolves) behind an
// httptest server and returns a live line channel for one open GET stream. The
// returned cancel tears the request down (unblocking the handler goroutine).
func streamHarness(t *testing.T, hub *Hub, runID, lastEventID string) (<-chan string, context.CancelFunc) {
	t.Helper()
	r := mux.NewRouter()
	r.HandleFunc("/api/runs/{runId}/stream", hub.streamRun).Methods(http.MethodGet)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	url := srv.URL + "/api/runs/" + runID + "/stream"
	if lastEventID != "" {
		url += "?lastEventId=" + lastEventID
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		cancel()
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("open stream: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })

	lines := make(chan string, 256)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	return lines, cancel
}

// waitLine reads until a line satisfying pred, failing on timeout / stream close.
func waitLine(t *testing.T, lines <-chan string, pred func(string) bool, what string) string {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatalf("stream closed while waiting for %s", what)
			}
			if pred(line) {
				return line
			}
		case <-deadline:
			t.Fatalf("timeout waiting for %s", what)
		}
	}
}

func hasPrefix(p string) func(string) bool {
	return func(s string) bool { return strings.HasPrefix(s, p) }
}
func isExactly(w string) func(string) bool { return func(s string) bool { return s == w } }

// stubUIDResolver is a name→UID resolver for the stream-route tests. A nil map
// (or a name not in it) resolves to "" so the handler keeps the raw name; err
// forces the best-effort fallback path.
type stubUIDResolver struct {
	byName map[string]string
	err    error
}

func (s stubUIDResolver) ResolveRunUID(_ context.Context, name string) (string, error) {
	if s.err != nil {
		return "", s.err
	}
	return s.byName[name], nil
}

// A fresh connection (no Last-Event-ID) now BACKFILLS the durable tail before it
// live-tails, so a terminal event published BEFORE the client connected — to zero
// subscribers — is still delivered instead of lost forever (ISI-5248 secondary
// fix: a fast run can 'end' before the ticket opens its EventSource).
func TestStreamRun_FreshConnectReplaysTail(t *testing.T) {
	hub := NewHub()
	reader := &fakeRunReader{}
	// A pre-connect terminal event: published while no subscriber was listening.
	reader.add(events.RunEvent{ID: 7, RunID: "run-1", EventType: "ended", Payload: []byte(`{"phase":"Succeeded"}`)})
	hub.SetReplayer(NewRunReplayer(reader))

	lines, cancel := streamHarness(t, hub, "run-1", "")
	defer cancel()

	waitLine(t, lines, isExactly(": subscribed run=run-1"), "subscribe preamble")
	// The pre-connect 'ended' is backfilled even without a Last-Event-ID.
	if got := waitLine(t, lines, hasPrefix("id: "), "backfilled terminal id"); got != "id: 7" {
		t.Fatalf("want fresh-connect backfill of pre-connect id 7, got %q", got)
	}
	waitLine(t, lines, isExactly("event: ended"), "ended event line")
}

// The stream route resolves the wire id (a run CR NAME) to the run UID BEFORE it
// subscribes, so its Subscribe key matches the projector's publish key (run.UID).
// Without this the ticket subscribes on the name and receives only keepalives —
// the pill sticks on 'working' forever (ISI-5248).
func TestStreamRun_ResolvesNameToUID(t *testing.T) {
	const uid = "11111111-1111-1111-1111-111111111111"
	hub := NewHub()
	hub.SetRunUIDResolver(stubUIDResolver{byName: map[string]string{"intake-abc-r1": uid}})

	lines, cancel := streamHarness(t, hub, "intake-abc-r1", "")
	defer cancel()

	// Preamble echoes the RESOLVED UID — the key the subscription is registered under.
	waitLine(t, lines, isExactly(": subscribed run="+uid), "resolved subscribe preamble")

	// An event published under the raw NAME reaches nobody; one under the UID (as the
	// projector publishes) reaches this subscriber.
	hub.Publish("intake-abc-r1", Event{ID: "1", Type: "thinking", Data: "wrong-key"})
	hub.Publish(uid, Event{ID: "2", Type: "thinking", Data: "right-key"})
	if got := waitLine(t, lines, hasPrefix("id: "), "event on resolved key"); got != "id: 2" {
		t.Fatalf("want event id 2 (published under resolved UID), got %q", got)
	}
	waitLine(t, lines, isExactly("data: right-key"), "right-key data")
}

// A resolver miss (unknown name / error) falls back to the raw name rather than
// failing the stream — the pre-ISI-5248 behavior, so UID-native/dev routes and
// tests keep working.
func TestStreamRun_ResolverMissFallsBackToName(t *testing.T) {
	hub := NewHub()
	hub.SetRunUIDResolver(stubUIDResolver{err: errors.New("not found")})

	lines, cancel := streamHarness(t, hub, "run-raw", "")
	defer cancel()

	waitLine(t, lines, isExactly(": subscribed run=run-raw"), "fallback subscribe preamble")
	hub.Publish("run-raw", Event{ID: "9", Type: "progress", Data: "live"})
	if got := waitLine(t, lines, hasPrefix("id: "), "live on fallback key"); got != "id: 9" {
		t.Fatalf("want id 9 on raw-name fallback, got %q", got)
	}
}

// With a Last-Event-ID, the durable tail after that id is replayed first, then the
// live channel is deduped against the highest replayed id (a re-published event is
// dropped) while newer ids pass through.
func TestStreamRun_ReplayThenDedupLiveTail(t *testing.T) {
	hub := NewHub()
	reader := &fakeRunReader{}
	reader.add(
		events.RunEvent{ID: 1, RunID: "run-1", EventType: "created", Payload: []byte(`{}`)},
		events.RunEvent{ID: 2, RunID: "run-1", EventType: "reconcile_advanced", Payload: []byte(`{"to_step":"x"}`)},
		events.RunEvent{ID: 3, RunID: "run-1", EventType: "reconcile_advanced", Payload: []byte(`{"to_step":"y"}`)},
	)
	hub.SetReplayer(NewRunReplayer(reader))

	// Reconnect with Last-Event-ID = 1 ⇒ replay ids 2 and 3.
	lines, cancel := streamHarness(t, hub, "run-1", "1")
	defer cancel()

	waitLine(t, lines, isExactly(": subscribed run=run-1"), "subscribe preamble")
	if got := waitLine(t, lines, hasPrefix("id: "), "replay id 2"); got != "id: 2" {
		t.Fatalf("want replay to start at id 2, got %q", got)
	}
	if got := waitLine(t, lines, hasPrefix("id: "), "replay id 3"); got != "id: 3" {
		t.Fatalf("want replay id 3, got %q", got)
	}

	// Now the live tail re-delivers id 3 (must be deduped) and a fresh id 4 (must pass).
	hub.Publish("run-1", Event{ID: "3", Type: "reconcile_advanced", Data: `{"to_step":"y"}`})
	hub.Publish("run-1", Event{ID: "4", Type: "completed", Data: `{"ok":true}`})
	if got := waitLine(t, lines, hasPrefix("id: "), "next live id"); got != "id: 4" {
		t.Fatalf("dedup failed: want next id 4 (id 3 already replayed), got %q", got)
	}
}

// A replay error degrades to a live tail rather than failing the stream.
func TestStreamRun_ReplayErrorDegradesToLive(t *testing.T) {
	hub := NewHub()
	reader := &fakeRunReader{errForRun: errors.New("bad uuid cast")}
	hub.SetReplayer(NewRunReplayer(reader))

	lines, cancel := streamHarness(t, hub, "run-1", "1")
	defer cancel()

	waitLine(t, lines, isExactly(": subscribed run=run-1"), "subscribe preamble")
	waitLine(t, lines, isExactly(": replay unavailable"), "replay-unavailable notice")

	hub.Publish("run-1", Event{ID: "8", Type: "progress", Data: "live"})
	if got := waitLine(t, lines, hasPrefix("id: "), "live id after degrade"); got != "id: 8" {
		t.Fatalf("want live id 8 after replay degrade, got %q", got)
	}
}

func TestParseLastEventID(t *testing.T) {
	newReq := func(header, query string) *http.Request {
		url := "/api/runs/r/stream"
		if query != "" {
			url += "?lastEventId=" + query
		}
		req := httptest.NewRequest(http.MethodGet, url, nil)
		if header != "" {
			req.Header.Set("Last-Event-ID", header)
		}
		return req
	}
	cases := []struct {
		name          string
		header, query string
		wantID        int64
		wantOK        bool
	}{
		{"none", "", "", 0, false},
		{"header int", "42", "", 42, true},
		{"query overrides header", "42", "7", 7, true},
		{"blank header", "", "", 0, false},
		{"non-numeric", "abc", "", 0, false},
		{"negative", "-1", "", 0, false},
		{"zero is valid", "0", "", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, ok := parseLastEventID(newReq(tc.header, tc.query))
			if id != tc.wantID || ok != tc.wantOK {
				t.Fatalf("parseLastEventID = (%d, %v), want (%d, %v)", id, ok, tc.wantID, tc.wantOK)
			}
		})
	}
}
