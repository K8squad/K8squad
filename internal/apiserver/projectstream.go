package apiserver

import (
	"fmt"
	"net/http"
	"regexp"
	"sync"
	"time"

	"github.com/gorilla/mux"
)

// ============================================================================
// Per-project SSE fan-out bus (ISI-5194 / S6, plan ISI-5185, design ISI-5186)
// ============================================================================
//
// The run Hub (sse.go) fans events out keyed by runID — the transport every per-run surface
// rides. But the ticket Activity view and the discussion Room are project-scoped: they want the
// live stream for a whole Project, not one Run. Before this bus there was NO server-side
// /api/projects/{id}/stream handler (the console BFF proxied to a 404), so those surfaces polled.
//
// ProjectHub is the convergent long-term transport: a subscriber opens ONE EventSource on
// GET /api/projects/{projectId}/stream and receives every event any producer publishes for that
// project — a discussion message appended, an agent working/thinking delta, a run status change.
// It is the same in-process fan-out shape as Hub, keyed by projectID instead of runID, and it
// reuses Hub's Event/subscriber/writeEvent primitives (declared in sse.go).
//
// Like Hub, ProjectHub owns transport only (ordering, buffering, slow-consumer policy) — never
// event production. A subscriber whose buffer overflows is dropped rather than blocking the
// publisher: fan-out is best-effort and a wedged browser must not stall a producer. Unlike Hub,
// there is no durable Last-Event-ID replay: the project stream is self-healing on reconnect (each
// consuming surface re-fetches its own snapshot over its REST read model, exactly as
// teamStatusStream does), so a live tail is the whole contract.

// projectIDPattern bounds the decoded {projectId} to a charset that can carry no CR/LF, so it can
// never inject extra SSE frames when echoed into the stream (gosec G705 / response splitting). A
// Project id is either a bare k8s name or the canonical "namespace/name" composite (ISI-3982 /
// ISI-4795); both are a subset of this charset (the '/' is the composite separator).
var projectIDPattern = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

// ProjectHub fans project-scoped events out to subscribed SSE connections. The zero value is not
// usable; use NewProjectHub. It is safe for concurrent Publish/Subscribe/Unsubscribe.
type ProjectHub struct {
	mu        sync.RWMutex
	subs      map[string]map[*subscriber]struct{} // projectID → set of live subscribers
	keepAlive time.Duration
}

// NewProjectHub builds an empty ProjectHub with the default keep-alive interval.
func NewProjectHub() *ProjectHub {
	return &ProjectHub{subs: make(map[string]map[*subscriber]struct{}), keepAlive: defaultKeepAlive}
}

// Subscribe registers a new subscriber for projectID and returns it. Callers MUST Unsubscribe when
// done (the stream handler defers it).
func (h *ProjectHub) Subscribe(projectID string) *subscriber {
	s := &subscriber{ch: make(chan Event, subBuffer)}
	h.mu.Lock()
	defer h.mu.Unlock()
	set, ok := h.subs[projectID]
	if !ok {
		set = make(map[*subscriber]struct{})
		h.subs[projectID] = set
	}
	set[s] = struct{}{}
	return s
}

// Unsubscribe removes s from projectID's fan-out set and closes its channel. Idempotent.
func (h *ProjectHub) Unsubscribe(projectID string, s *subscriber) {
	h.mu.Lock()
	defer h.mu.Unlock()
	set, ok := h.subs[projectID]
	if !ok {
		return
	}
	if _, ok := set[s]; ok {
		delete(set, s)
		close(s.ch)
	}
	if len(set) == 0 {
		delete(h.subs, projectID)
	}
}

// Publish fans event out to every current subscriber of projectID. It NEVER blocks: a subscriber
// whose buffer is full is skipped (best-effort delivery), so one slow browser can neither stall a
// producer nor delay another subscriber. Returns the number of subscribers the event reached — a
// publisher can use a zero return to skip more expensive work when nobody is watching.
func (h *ProjectHub) Publish(projectID string, event Event) int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	delivered := 0
	for s := range h.subs[projectID] {
		select {
		case s.ch <- event:
			delivered++
		default:
			// slow consumer — drop this event for this subscriber rather than block
		}
	}
	return delivered
}

// subscriberCount reports live subscribers for projectID (test/introspection helper).
func (h *ProjectHub) subscriberCount(projectID string) int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.subs[projectID])
}

// streamProject is the GET /api/projects/{projectId}/stream handler. It is mounted behind the §13
// authz choke point AND, when a membership resolver is wired, requireProjectRole(viewer) — so it
// only runs for a caller who may already read the Project (same existence-hiding tenancy as the
// dashboard). It upgrades the connection to text/event-stream and live-tails events for the
// project until the client disconnects; there is no durable replay (see the type comment).
func (h *ProjectHub) streamProject(w http.ResponseWriter, r *http.Request) {
	projectID := decodePathVar(mux.Vars(r)["projectId"])
	if projectID == "" {
		writeJSONError(w, http.StatusBadRequest, "missing projectId")
		return
	}
	if !projectIDPattern.MatchString(projectID) {
		// Reject before any stream write so a CR/LF-bearing projectId can never inject SSE frames.
		writeJSONError(w, http.StatusBadRequest, "invalid projectId")
		return
	}

	// Headers MUST be set before the first flush — the initial flush commits the status line and
	// header block, after which any header change is ignored. ResponseController.Flush works across
	// wrapped ResponseWriters (middleware) where a bare http.Flusher type-assert would fail.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // defeat nginx/proxy response buffering
	w.WriteHeader(http.StatusOK)

	rc := http.NewResponseController(w)
	if err := rc.Flush(); err != nil {
		return // streaming unsupported by this writer; status already sent
	}

	// Subscribe before announcing the open stream so no event published in the gap is lost.
	sub := h.Subscribe(projectID)
	defer h.Unsubscribe(projectID, sub)

	// Announce the open stream so a client (and tests) can confirm the tail is live immediately,
	// before the first domain event. This is a comment line — inert to EventSource `message`.
	// projectID was validated against projectIDPattern above (no CR/LF), so this echo cannot inject
	// SSE frames; gosec's taint engine can't see the regexp guard, hence the suppression.
	// #nosec G705 -- projectID is charset-validated above; no CR/LF can reach the stream.
	fmt.Fprintf(w, ": subscribed project=%s\n\n", projectID)
	_ = rc.Flush()

	ctx := r.Context()

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
			if err := writeEvent(w, ev); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
		}
	}
}
