package apiserver

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/gorilla/mux"

	"github.com/K8squad/K8squad/internal/discussion"
)

// ============================================================================
// The apiserver HTTP host (§17.3) — the binary the console BFF proxies to
// ============================================================================
//
// This is the parent router internal/discussion's Handler.Mount doc calls "the parent router
// the apiserver makes". It is the ONE gorilla/mux root that:
//   1. mounts the §7.5 discussion surface behind the §13 BFF authz choke point,
//   2. hosts the §4.4 SSE run-progress hub at GET /api/runs/{id}/stream (8.2),
//   3. exposes health/readiness for the Deployment probes,
//   4. declares the build-browser (8.7d) and squad-overview (8.1) read-model routes as
//      documented 501s until their backing read models land (see the ISI-2750 child issues) —
//      an honest, discoverable contract rather than a 404 the BFF cannot distinguish from a bug.
//
// Identity: the router trusts NOTHING from the caller except the forwarded ksquad_session cookie,
// which the CookieAuthenticator resolves into the server-derived AuthorContext (auth.go). Every
// gated route rides discussion.BFFAuthz(authenticator).

// ReadinessChecker reports whether a backing dependency (e.g. Postgres) is serving. The host
// gates /readyz on it; /healthz is liveness-only and never depends on it (a DB blip must not
// trigger a pod restart loop).
type ReadinessChecker interface {
	Ready(ctx context.Context) error
}

// Server bundles the root router and its dependencies. Build it with NewServer and serve
// Server.Handler() from an http.Server.
type Server struct {
	router *mux.Router
	hub    *Hub
}

// Options wires the host's collaborators. Authenticator and Discussion are required for the
// gated surface; Ready is optional (nil ⇒ /readyz always 200, for a DB-less dev run); Overview is
// optional (nil ⇒ GET /api/squad/overview keeps its documented 501 until the informer cache is
// wired, for a cluster-less dev run).
type Options struct {
	Authenticator discussion.Authenticator
	Discussion    *discussion.Handler
	Ready         ReadinessChecker
	Overview      SquadOverviewReader // 8.1 squad-overview read model; nil ⇒ documented 501
	Hub           *Hub                // optional; NewServer allocates one when nil
}

// NewServer assembles the root router from opts.
func NewServer(opts Options) *Server {
	hub := opts.Hub
	if hub == nil {
		hub = NewHub()
	}
	s := &Server{router: mux.NewRouter(), hub: hub}
	s.routes(opts)
	return s
}

// Handler returns the root http.Handler to serve.
func (s *Server) Handler() http.Handler { return s.router }

// Hub returns the SSE hub so a publisher (run reconciler / outbox relay) can fan events out.
func (s *Server) Hub() *Hub { return s.hub }

func (s *Server) routes(opts Options) {
	// ── Liveness / readiness (unauthenticated; probes only, no tenant data) ──
	s.router.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}).Methods(http.MethodGet)

	s.router.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if opts.Ready != nil {
			if err := opts.Ready.Ready(r.Context()); err != nil {
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not-ready", "error": err.Error()})
				return
			}
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	}).Methods(http.MethodGet)

	// ── Gated API surface (everything below rides the §13 authz choke point) ──
	// The discussion Handler installs its own subrouter+BFFAuthz via Mount; the SSE and
	// read-model routes are gated explicitly with the same middleware so identity resolution
	// is uniform across the host.
	if opts.Discussion != nil && opts.Authenticator != nil {
		opts.Discussion.Mount(s.router, opts.Authenticator)
	}

	if opts.Authenticator != nil {
		authz := discussion.BFFAuthz(opts.Authenticator)

		// §4.4 / 8.2 — the one run-progress SSE stream every live surface rides.
		stream := s.router.Path("/api/runs/{runId}/stream").Subrouter()
		stream.Use(authz)
		stream.HandleFunc("", s.hub.streamRun).Methods(http.MethodGet)

		// 8.7d build-browser: route exists and authorizes, but its backing read model is
		// not yet built. It answers a documented 501 so the BFF receives an honest,
		// distinguishable response (see notImplemented).
		build := s.router.Path("/api/runs/{runId}/build/{resource}").Subrouter()
		build.Use(authz)
		build.HandleFunc("", notImplemented("build-browser read model", "ISI-2750 child: build-browser read endpoints (8.7a/8.7d)")).
			Methods(http.MethodGet)

		// 8.1 squad-overview: served by the Team→Project→Run-status read model (overview.go,
		// ISI-2760) when the informer cache is wired. Absent a reader (cluster-less dev run)
		// it keeps the documented 501 so the contract stays honest.
		squad := s.router.Path("/api/squad/overview").Subrouter()
		squad.Use(authz)
		if opts.Overview != nil {
			squad.HandleFunc("", s.squadOverview(opts.Overview)).Methods(http.MethodGet)
		} else {
			squad.HandleFunc("", notImplemented("squad-overview read model", "ISI-2760: squad-overview read model (8.1)")).
				Methods(http.MethodGet)
		}
	}
}

// notImplemented returns a handler that answers 501 with a machine-readable body naming the
// missing read model and the issue tracking it. This is deliberately NOT a 404: the route is
// part of the host's contract; only its backing is pending. The BFF can surface "not yet
// available" distinctly from "unknown route / bug".
func notImplemented(what, tracking string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusNotImplemented, map[string]string{
			"error":    "not implemented",
			"detail":   what + " is not yet hosted by the apiserver",
			"tracking": tracking,
		})
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
