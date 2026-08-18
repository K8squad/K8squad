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

func hasPrefix(p string) func(string) bool { return func(s string) bool { return strings.HasPrefix(s, p) } }
func isExactly(w string) func(string) bool { return func(s string) bool { return s == w } }

// A fresh connection (no Last-Event-ID) gets the preamble, replays nothing, and
// receives a subsequently-published live event.
func TestStreamRun_LiveTailNoReplay(t *testing.T) {
	hub := NewHub()
	// A replayer that would return history — it MUST NOT be consulted without a Last-Event-ID.
	reader := &fakeRunReader{}
	reader.add(events.RunEvent{ID: 1, RunID: "run-1", EventType: "created", Payload: []byte(`{}`)})
	hub.SetReplayer(NewRunReplayer(reader))

	lines, cancel := streamHarness(t, hub, "run-1", "")
	defer cancel()

	// Preamble confirms the subscription is live before we publish.
	waitLine(t, lines, isExactly(": subscribed run=run-1"), "subscribe preamble")

	hub.Publish("run-1", Event{ID: "5", Type: "progress", Data: "hello"})
	if got := waitLine(t, lines, hasPrefix("id: "), "first id line"); got != "id: 5" {
		t.Fatalf("want live id 5 (no history replay), got %q", got)
	}
	waitLine(t, lines, isExactly("event: progress"), "event line")
	waitLine(t, lines, isExactly("data: hello"), "data line")
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
