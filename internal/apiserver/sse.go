package apiserver

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/mux"
)

// runIDPattern bounds the {runId} path var to an opaque-identifier charset. It can carry no CR/LF,
// so it can never inject extra SSE frames when echoed into the stream (gosec G705 / HTTP response
// splitting). Any run identifier the platform mints (UUID/ULID/k8s name) is a subset of this.
var runIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// ============================================================================
// SSE progress hub (§4.4 / story 8.2) — the ONE run-stream transport
// ============================================================================
//
// Every live console surface (8.2 run progress, 8.8f tiles, 8.10/8.11 detail) rides a single
// EventSource the BFF proxies to GET /api/runs/{id}/stream. This Hub is the in-process fan-out:
// publishers (the run reconciler / outbox relay, wired by the SSE-source child issue) call
// Publish(runID, event); each subscribed HTTP connection for that run receives it.
//
// The Hub owns transport only — ordering, buffering, and slow-consumer policy — never event
// production. A subscriber whose buffer overflows is dropped (its stream ends) rather than
// blocking the publisher: progress is best-effort and a wedged browser must not stall a Run.

// Event is one server-sent event. ID (optional) becomes the SSE `id:` line so a reconnecting
// client can resume via Last-Event-ID; Type becomes `event:`; Data is the JSON/text payload.
type Event struct {
	ID   string
	Type string
	Data string
}

const (
	// subBuffer bounds a single subscriber's backlog before it is declared a slow consumer
	// and dropped. Sized for a bursty-but-transient reader; a browser that falls this far
	// behind has effectively disconnected.
	subBuffer = 64
	// defaultKeepAlive is how often an idle stream emits an SSE comment (`: ping`) so proxies
	// and load balancers do not reap the connection. Overridable per Hub for tests.
	defaultKeepAlive = 20 * time.Second
)

type subscriber struct {
	ch chan Event
}

// runReplayer supplies the durable per-run event tail so a reconnecting client can resume from
// its Last-Event-ID. It is the read side of coord.outbox (events.RunEventReader, adapted in
// runevents.go); nil ⇒ the Hub live-tails only (no replay), the original behavior. Replay is
// best-effort: an error is logged and the stream continues live, never failing the connection.
type runReplayer interface {
	// replayRun returns this run's events with SSE id > afterID, ascending. The returned Events
	// carry the outbox row id as Event.ID so the client can resume again from the last one.
	replayRun(ctx context.Context, runID string, afterID int64) ([]Event, error)
}

// runUIDResolver maps a run CR NAME — the id the console threads on the wire (GET /api/runs
// returns run.Name; useDispatchWatch/useRunStream forward it verbatim) — to the run UID the Hub
// publishes under. The projector fans run events keyed by coord.outbox run_id = the mint's RunID
// claim = run.UID (runevents.go / progressmirror / prodreconcile), NOT the name, so the stream
// route MUST resolve name→UID before Subscribe/replay or it subscribes on a key nothing ever
// publishes to and the client sees only keepalives — the pill sticks on 'working' forever
// (ISI-5248, same name/UID class as ISI-5209). nil ⇒ the Hub keys on the raw path var (DB-less/dev
// routes, and tests that already pass a UID). Best-effort: a resolver error leaves the raw name as
// the key rather than failing the stream.
type runUIDResolver interface {
	ResolveRunUID(ctx context.Context, name string) (string, error)
}

// Hub fans run events out to subscribed SSE connections. The zero value is not usable; use
// NewHub. It is safe for concurrent Publish/Subscribe/Unsubscribe.
type Hub struct {
	mu        sync.RWMutex
	subs      map[string]map[*subscriber]struct{} // runID → set of live subscribers
	keepAlive time.Duration
	replay    runReplayer    // optional; nil ⇒ live-tail only (no Last-Event-ID replay)
	resolve   runUIDResolver // optional; nil ⇒ subscribe under the raw path var (name==UID)
}

// NewHub builds an empty Hub with the default keep-alive interval.
func NewHub() *Hub {
	return &Hub{subs: make(map[string]map[*subscriber]struct{}), keepAlive: defaultKeepAlive}
}

// SetReplayer wires the durable per-run tail used for Last-Event-ID replay. Called once at
// wiring time (main.go / NewServer) before the Hub serves; nil leaves the Hub live-tail only.
func (h *Hub) SetReplayer(r runReplayer) { h.replay = r }

// SetRunUIDResolver wires the run NAME→UID resolver the stream route uses to match its Subscribe
// key to the publish key (ISI-5248). Called once at wiring time (NewServer) before the Hub serves;
// nil leaves the Hub keyed on the raw path var.
func (h *Hub) SetRunUIDResolver(r runUIDResolver) { h.resolve = r }

// Subscribe registers a new subscriber for runID and returns it. Callers MUST Unsubscribe when
// done (the stream handler defers it).
func (h *Hub) Subscribe(runID string) *subscriber {
	s := &subscriber{ch: make(chan Event, subBuffer)}
	h.mu.Lock()
	defer h.mu.Unlock()
	set, ok := h.subs[runID]
	if !ok {
		set = make(map[*subscriber]struct{})
		h.subs[runID] = set
	}
	set[s] = struct{}{}
	return s
}

// Unsubscribe removes s from runID's fan-out set and closes its channel. Idempotent.
func (h *Hub) Unsubscribe(runID string, s *subscriber) {
	h.mu.Lock()
	defer h.mu.Unlock()
	set, ok := h.subs[runID]
	if !ok {
		return
	}
	if _, ok := set[s]; ok {
		delete(set, s)
		close(s.ch)
	}
	if len(set) == 0 {
		delete(h.subs, runID)
	}
}

// Publish fans event out to every current subscriber of runID. It NEVER blocks: a subscriber
// whose buffer is full is skipped (best-effort delivery), so one slow browser can neither stall
// the publisher nor delay another subscriber. Returns the number of subscribers the event was
// delivered to.
func (h *Hub) Publish(runID string, event Event) int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	delivered := 0
	for s := range h.subs[runID] {
		select {
		case s.ch <- event:
			delivered++
		default:
			// slow consumer — drop this event for this subscriber rather than block
		}
	}
	return delivered
}

// subscriberCount reports live subscribers for runID (test/introspection helper).
func (h *Hub) subscriberCount(runID string) int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.subs[runID])
}

// streamRun is the GET /api/runs/{id}/stream handler. It is mounted behind the §13 authz choke
// point, so it only runs for an authenticated caller; per-run authorization (does this principal
// own/see this Run) is enforced by the resolver's Team scope once the run→team read model lands
// (SSE-source child issue). It upgrades the connection to text/event-stream, replays nothing
// (live-tail), and streams events until the client disconnects.
func (h *Hub) streamRun(w http.ResponseWriter, r *http.Request) {
	runID := decodePathVar(mux.Vars(r)["runId"])
	if runID == "" {
		writeJSONError(w, http.StatusBadRequest, "missing runId")
		return
	}
	if !runIDPattern.MatchString(runID) {
		// Reject before any stream write so a CR/LF-bearing runId can never inject SSE frames.
		writeJSONError(w, http.StatusBadRequest, "invalid runId")
		return
	}

	ctx := r.Context()

	// Resolve the wire id (a run CR NAME) to the run UID the Hub publishes under (ISI-5248). The
	// projector fans events keyed by run_id = run.UID, but the console threads run.Name, so without
	// this the Subscribe/replay key never matches the publish key and the client sees only
	// keepalives. Best-effort: a resolver miss (missing/forbidden run, transient read, or a
	// non-conforming UID) falls back to the raw name — the pre-ISI-5248 behavior — rather than
	// failing the stream, so DB-less/dev routes and UID-native callers still work. subKey stays
	// charset-validated (a UUID is a subset of runIDPattern) so the ': subscribed' echo below can
	// never inject SSE frames.
	subKey := runID
	if h.resolve != nil {
		if uid, err := h.resolve.ResolveRunUID(ctx, runID); err == nil && uid != "" && runIDPattern.MatchString(uid) {
			subKey = uid
		}
	}

	// Headers MUST be set before the first flush — the initial flush commits the status line and
	// header block, after which any header change is ignored (net/http logs a superfluous-header
	// warning). ResponseController.Flush works across wrapped ResponseWriters (middleware) where a
	// bare type-assert to http.Flusher would fail.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // defeat nginx/proxy response buffering
	w.WriteHeader(http.StatusOK)

	rc := http.NewResponseController(w)
	if err := rc.Flush(); err != nil {
		return // streaming unsupported by this writer; status already sent
	}

	// Subscribe BEFORE replay so any event committed during the replay query is buffered on the
	// live channel rather than lost in the gap between the replay snapshot and the tail. The two
	// feeds then overlap by at most a bounded suffix, which the id-dedup below collapses exactly.
	sub := h.Subscribe(subKey)
	defer h.Unsubscribe(subKey, sub)

	// Announce the open stream so a client (and tests) can confirm the tail is live immediately,
	// before the first domain event. This is a comment line — inert to EventSource `message`.
	// subKey was validated against runIDPattern above (no CR/LF), so this echo cannot inject SSE
	// frames; gosec's taint engine can't see the regexp guard, hence the suppression.
	// #nosec G705 -- subKey is charset-validated above; no CR/LF can reach the stream.
	fmt.Fprintf(w, ": subscribed run=%s\n\n", subKey)
	_ = rc.Flush()

	// Durable-tail replay (§4.4). On reconnect EventSource resends the last `id:` it saw via the
	// Last-Event-ID header (a `?lastEventId=` query overrides it for non-EventSource clients and
	// tests); we replay STRICTLY AFTER that id. A FRESH connection (no/blank/invalid id) replays
	// the tail from the start (afterID=0, bounded by the store's per-run cap) rather than nothing:
	// the ticket opens its EventSource only after a <=2s discovery poll, so a fast run can publish
	// its terminal 'ended' BEFORE first connect — to zero subscribers — and the pill would stick on
	// 'working' forever (ISI-5248). Backfilling on connect delivers that pre-connect event. Either
	// way we dedup the live channel against the highest id replayed so an event carried by both
	// feeds is written once; Subscribe-before-replay bounds the overlap to a suffix the id-dedup
	// collapses exactly.
	replayedThrough := int64(-1)
	if h.replay != nil {
		afterID, _ := parseLastEventID(r) // (0, false) on a fresh connect ⇒ replay the full tail
		evs, err := h.replay.replayRun(ctx, subKey, afterID)
		if err != nil {
			// Best-effort: a replay failure (e.g. a non-uuid subKey from the fallback path, a DB
			// blip) must not fail the stream. Fall through to a live tail; the client simply misses
			// backfill this connect.
			fmt.Fprintf(w, ": replay unavailable\n\n")
			_ = rc.Flush()
		} else {
			for _, ev := range evs {
				if err := writeEvent(w, ev); err != nil {
					return
				}
				if id, perr := strconv.ParseInt(ev.ID, 10, 64); perr == nil && id > replayedThrough {
					replayedThrough = id
				}
			}
			if len(evs) > 0 {
				if err := rc.Flush(); err != nil {
					return
				}
			}
		}
	}

	ka := h.keepAlive
	if ka <= 0 {
		ka = defaultKeepAlive
	}
	ticker := time.NewTicker(ka)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return // client disconnected
		case <-ticker.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
		case ev, ok := <-sub.ch:
			if !ok {
				return // unsubscribed
			}
			// Dedup: drop a live event already delivered by replay (id <= replayedThrough). Events
			// with an unparseable/empty id are never deduped — they are passed through unchanged.
			if id, perr := strconv.ParseInt(ev.ID, 10, 64); perr == nil && id <= replayedThrough {
				continue
			}
			if err := writeEvent(w, ev); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
		}
	}
}

// parseLastEventID reads the reconnect cursor from the Last-Event-ID header, or a ?lastEventId=
// query override (non-EventSource clients / tests). It returns (afterID, true) only for a
// non-negative integer; a missing, blank, or malformed value yields (0, false) ⇒ no replay.
func parseLastEventID(r *http.Request) (int64, bool) {
	raw := r.Header.Get("Last-Event-ID")
	if q := r.URL.Query().Get("lastEventId"); q != "" {
		raw = q
	}
	if raw == "" {
		return 0, false
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id < 0 {
		return 0, false
	}
	return id, true
}

// writeEvent serializes one Event in SSE wire format. Multi-line data is split into successive
// `data:` lines per the spec so embedded newlines do not terminate the event.
func writeEvent(w http.ResponseWriter, ev Event) error {
	if ev.ID != "" {
		if _, err := fmt.Fprintf(w, "id: %s\n", ev.ID); err != nil {
			return err
		}
	}
	if ev.Type != "" {
		if _, err := fmt.Fprintf(w, "event: %s\n", ev.Type); err != nil {
			return err
		}
	}
	// Split on \n so multi-line payloads stay one SSE event.
	start := 0
	data := ev.Data
	for i := 0; i <= len(data); i++ {
		if i == len(data) || data[i] == '\n' {
			if _, err := fmt.Fprintf(w, "data: %s\n", data[start:i]); err != nil {
				return err
			}
			start = i + 1
		}
	}
	_, err := fmt.Fprint(w, "\n") // blank line terminates the event
	return err
}
