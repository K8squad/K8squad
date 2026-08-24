package apiserver

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/gorilla/mux"

	"github.com/K8squad/K8squad/internal/artifactbrowser"
	"github.com/K8squad/K8squad/internal/buildbrowser"
	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/K8squad/K8squad/pkg/auth"
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
	// Credentials is the 8.6 credential/auth-state read model; nil ⇒ GET /api/credentials
	// keeps its documented 501 (cluster-less dev run), exactly like Overview.
	Credentials CredentialOverviewReader // 8.6 credential read model; nil ⇒ documented 501
	Hub         *Hub                     // optional; NewServer allocates one when nil
	// Builds is the 8.7a build-browser read-model (behind the 8.7d gate, ISI-2759). When nil the
	// build routes keep answering the documented 501 (dev run without a Run source wired).
	Builds *buildbrowser.Service
	// Dashboard is the 8.8a per-Project dashboard read model (ISI-2906). When nil the dashboard
	// route keeps answering the documented 501 (dev run without an informer cache wired).
	Dashboard *DashboardService
	// Artifacts is the 8.3 artifact-browser read-model (coordination-record blobs + handoff
	// outputs, ISI-2900). When nil the artifact routes keep answering the documented 501
	// (dev run without the coord store wired).
	Artifacts *artifactbrowser.Service
	// Auth is the Epic 15 identity seam (15.1 /auth/* + 15.2 /admin/users, ISI-2920).
	// A zero Service ⇒ the routes are not mounted (pre-Epic-15 host shape).
	Auth AuthRoutesOptions
	// AuditTrail is the 2.6 audit-log read model (coord.audit_log query surface,
	// ISI-2881). Nil ⇒ GET /api/audit keeps its documented 501 (a DB-less dev run),
	// exactly like the other read models.
	AuditTrail AuditTrailReader
	// WorkItemState is the 8.14a human board-lane transition op (coord.HumanStateStore,
	// ISI-2909). Nil ⇒ PATCH /api/work-items/{id}/state keeps its documented 501 (a
	// DB-less dev run), exactly like the read models above.
	WorkItemState WorkItemStateTransitioner
	// ProjectRoles is the 15.4 per-Project RBAC enforcement seam (auth.project_membership,
	// ISI-2921). When set, project-scoped routes gain a requireProjectRole gate INSIDE the §13
	// choke point (admin bypasses; a caller with no membership gets 404 existence-hiding; an
	// under-privileged member gets 403). Nil ⇒ the routes keep the pre-15.4 shape (team-scope
	// checks only), so a DB-less dev run and existing deployments are unchanged.
	ProjectRoles ProjectRoleResolver
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

	// Epic 15 identity seam (ISI-2920): /auth/* is the cookie ISSUER (login answers
	// unauthenticated; refresh/logout/me resolve the cookie themselves), and
	// /admin/users rides the requireAdmin gate. Mounted before the gated surface so
	// a route conflict fails loudly at assembly.
	s.mountAuthRoutes(opts.Auth)

	if opts.Discussion != nil && opts.Authenticator != nil {
		opts.Discussion.Mount(s.router, opts.Authenticator)
	}

	if opts.Authenticator != nil {
		authz := discussion.BFFAuthz(opts.Authenticator)

		// §4.4 / 8.2 — the one run-progress SSE stream every live surface rides.
		stream := s.router.Path("/api/runs/{runId}/stream").Subrouter()
		stream.Use(authz)
		stream.HandleFunc("", s.hub.streamRun).Methods(http.MethodGet)

		// 8.7a/8.7d build-browser (ISI-2759): the git read-model behind the per-principal +
		// Team-scope gate. When a Run source is wired (opts.Builds != nil) the routes serve real
		// tree/diff/file/meta reads; otherwise they keep the documented 501 for a DB-less dev run.
		build := s.router.Path("/api/runs/{runId}/build/{resource}").Subrouter()
		build.Use(authz)
		if opts.Builds != nil {
			build.HandleFunc("", buildHandler(opts.Builds)).Methods(http.MethodGet)
		} else {
			build.HandleFunc("", notImplemented("build-browser read model", "ISI-2759: wire a buildbrowser.Service (RunSource) to enable")).
				Methods(http.MethodGet)
		}

		// 8.3 artifact browser (ISI-2900): the coordination-record view of a Run's artifacts —
		// blobs via digest-verified uris plus the parsed structured handoff (story 2.8). When the
		// coord store is wired (opts.Artifacts != nil) the routes serve real reads; otherwise they
		// keep the documented 501 for a DB-less dev run.
		arts := s.router.Path("/api/runs/{runId}/artifacts").Subrouter()
		arts.Use(authz)
		if opts.Artifacts != nil {
			arts.HandleFunc("", artifactsHandler(opts.Artifacts)).Methods(http.MethodGet)
		} else {
			arts.HandleFunc("", notImplemented("artifact-browser read model", "ISI-2900: wire a RunSource (KSQUAD_DEV_RUNS or the prod source, ISI-2759) to enable — the coord store is already wired")).
				Methods(http.MethodGet)
		}
		art := s.router.Path("/api/runs/{runId}/artifacts/{artifactId}").Subrouter()
		art.Use(authz)
		if opts.Artifacts != nil {
			art.HandleFunc("", artifactContentHandler(opts.Artifacts)).Methods(http.MethodGet)
		} else {
			art.HandleFunc("", notImplemented("artifact-browser read model", "ISI-2900: wire a RunSource (KSQUAD_DEV_RUNS or the prod source, ISI-2759) to enable — the coord store is already wired")).
				Methods(http.MethodGet)
		}

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

		// 8.8a per-Project dashboard: the ONE composed payload every 8.8b–8.8f tile draws from
		// (dashboard.go, ISI-2906), behind the SAME §12.3 choke point — no dashboard-specific
		// authz path. Nil service (cluster-less dev run) keeps the documented 501.
		dash := s.router.Path("/api/projects/{projectId}/dashboard").Subrouter()
		dash.Use(authz)
		// 15.4 RBAC (ISI-2921): when a membership resolver is wired, the dashboard — the first
		// project-scoped read surface — requires at least viewer on {projectId}. Admin bypasses;
		// a non-member gets 404 (existence-hiding, matching the handler's own foreign-Project 404).
		// Nil resolver ⇒ unchanged (team-scope checks in the handler still apply).
		if opts.ProjectRoles != nil {
			dash.Use(requireProjectRole(opts.ProjectRoles, auth.ProjectRoleViewer))
		}
		if opts.Dashboard != nil {
			dash.HandleFunc("", s.projectDashboard(opts.Dashboard)).Methods(http.MethodGet)
		} else {
			dash.HandleFunc("", notImplemented("project-dashboard read model", "ISI-2906: wire a DashboardService (informer cache) to enable")).
				Methods(http.MethodGet)
		}

		// 8.6 credential/auth-state (ISI-2902): the per-agent BYO-credential surface behind the
		// same choke point. A wired reader serves the Team-scoped projection; a cluster-less
		// dev run keeps the documented 501. POST /api/credentials/connect is the 7.7
		// Connect-Claude seam and answers its own documented 501 until ISI-2899 lands the
		// OAuth flow — the route exists so the console has one honest endpoint, never a
		// fabricated login.
		creds := s.router.Path("/api/credentials").Subrouter()
		creds.Use(authz)
		if opts.Credentials != nil {
			creds.HandleFunc("", s.credentials(opts.Credentials)).Methods(http.MethodGet)
		} else {
			creds.HandleFunc("", notImplemented("credential read model", "ISI-2902: wire a CredentialOverviewReader (informer cache) to enable")).
				Methods(http.MethodGet)
		}
		connect := s.router.Path("/api/credentials/connect").Subrouter()
		connect.Use(authz)
		connect.HandleFunc("", s.connectClaude()).Methods(http.MethodPost)

		// 2.6 audit-trail query API (ISI-2881): the read side of coord.audit_log —
		// who/what/when/result across work items, actors, and time. Behind the same
		// choke point; the handler applies the admin/self RBAC scoping. Wired to the
		// Postgres reader in prod (the DB is a hard start dependency), documented 501
		// only for a reader-less host shape.
		audit := s.router.Path("/api/audit").Subrouter()
		audit.Use(authz)
		if opts.AuditTrail != nil {
			audit.HandleFunc("", auditTrailHandler(opts.AuditTrail)).Methods(http.MethodGet)
		} else {
			audit.HandleFunc("", notImplemented("audit-trail read model", "ISI-2881: wire an AuditTrailReader (Postgres) to enable")).
				Methods(http.MethodGet)
		}

		// 8.14a human board-lane transition (ISI-2909): the write side of the §8.6/§13
		// board — PATCH /api/work-items/{id}/state. Behind the same choke point; the
		// handler applies the human-only RBAC and the store applies Team scoping +
		// no-fence audit (ADR-037). Wired to coord.HumanStateStore in prod (the DB is a
		// hard start dependency), documented 501 only for a store-less host shape.
		workItemState := s.router.Path("/api/work-items/{id}/state").Subrouter()
		workItemState.Use(authz)
		if opts.WorkItemState != nil {
			workItemState.HandleFunc("", workItemStateHandler(opts.WorkItemState)).Methods(http.MethodPatch)
		} else {
			workItemState.HandleFunc("", notImplemented("work-item state transition", "ISI-2909: wire a coord.HumanStateStore (Postgres) to enable")).
				Methods(http.MethodPatch)
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
