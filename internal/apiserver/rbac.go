package apiserver

// Epic 15.4 per-Project RBAC enforcement (ADR-035, ISI-2921).
//
// requireProjectRole is the middleware that turns the 15.3 membership store (auth.project_membership)
// into an access decision on a project-scoped route. It sits INSIDE the §13 choke point (BFFAuthz),
// so the caller's AuthorContext is already resolved and stamped on the request context — this
// middleware never re-authenticates; it authorizes.
//
// Decision order (fail-closed):
//  1. No AuthorContext / empty principal   → 401 (defence in depth; BFFAuthz already 401s upstream).
//  2. global_role=admin                     → allow. Admin is fleet-wide authority (0008); it needs
//                                             NO membership row and short-circuits the store lookup.
//  3. Resolve the caller's role on {projectId}. No membership → 404 (existence-hiding: a caller with
//     no grant is told nothing about whether the Project exists — the SAME shape dashboard.go uses
//     for a foreign/unknown Project).
//  4. Role rank < required                  → 403 (the caller demonstrably has *a* grant but not a
//     strong enough one — 403 is honest here because membership itself is not the secret).
//  5. Role rank ≥ required                  → allow.

import (
	"context"
	"errors"
	"log"
	"net/http"

	"github.com/gorilla/mux"

	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/K8squad/K8squad/pkg/auth"
)

// ProjectRoleResolver is the read seam the RBAC middleware needs: a caller's identity string → their
// role on a Project (auth.ErrNoMembership when they hold none). auth.PostgresMembershipStore
// satisfies it via RoleForPrincipal; unit tests supply a map-backed fake.
type ProjectRoleResolver interface {
	RoleForPrincipal(ctx context.Context, principal, project string) (string, error)
}

// requireProjectRole builds the 15.4 enforcement middleware for a project-scoped subrouter. The
// {projectId} path variable names the Project (same var the dashboard route binds). minRole is the
// weakest role that may proceed (ADR-035: viewer < contributor < maintainer).
func requireProjectRole(resolver ProjectRoleResolver, minRole string) mux.MiddlewareFunc {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if resolver == nil {
				// A mounted-but-unwired resolver must not fail open.
				writeJSONError(w, http.StatusServiceUnavailable, "authorization unavailable")
				return
			}
			author, ok := discussion.AuthFromContext(r.Context())
			if !ok || author.Principal == "" {
				writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
				return
			}
			if author.IsAdmin {
				next.ServeHTTP(w, r) // fleet-wide authority: no membership needed
				return
			}
			project := mux.Vars(r)["projectId"]
			if project == "" {
				// A project-scoped route with no bound {projectId} is a wiring bug — fail closed.
				writeJSONError(w, http.StatusNotFound, "no such project")
				return
			}
			role, err := resolver.RoleForPrincipal(r.Context(), author.Principal, project)
			switch {
			case errors.Is(err, auth.ErrNoMembership):
				// Existence-hiding: no grant ⇒ the Project does not exist for this caller.
				writeJSONError(w, http.StatusNotFound, "no such project")
				return
			case err != nil:
				log.Printf("apiserver: rbac resolve role for %s on %s: %v", author.Principal, project, err)
				writeJSONError(w, http.StatusBadGateway, "authorization check unavailable")
				return
			}
			if !auth.RoleAtLeast(role, minRole) {
				writeJSONError(w, http.StatusForbidden, "insufficient project role")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
