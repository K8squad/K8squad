package discussion

import (
	"context"
	"net/http"

	"github.com/google/uuid"
)

// ============================================================================
// Room tenancy scope — the project's OWNING Team, not the caller's session Team (ISI-5198).
// ============================================================================
//
// A discussion room IS a Project (R1), so its tenancy root is the Team that OWNS the Project's
// namespace — NOT the Team the calling session happens to be scoped to. These two coincide for the
// common caller (a project-team member), so the original write path stamped a thread's team_id from
// auth.TeamID and every read/write fenced on it. But a fleet-admin (or any principal whose session
// Team ≠ the project's Team) creating a thread stamped it with the WRONG Team, and a Run scoped to the
// project's Team could then not post into it — the reply was rejected on a team-scope mismatch
// (ISI-5198, split out of ISI-5189). The fix resolves the room's tenancy from the {projectId} path and
// fences every operation on THAT Team, so the room's scope is stable regardless of who opened it.

// ProjectTeamResolver maps a discussion room's {projectId} path variable to the UID of the Team that
// OWNS the Project's namespace (§12.1 "a squad IS a namespace"). ok=false when no Team claims the
// namespace (a DB-less dev host, or a partially-composed squad) — the handler then falls back to the
// caller's own Team scope, preserving the pre-ISI-5198 behaviour for setups without a resolver. The
// apiserver supplies the implementation over the shared informer cache (the same ProjectRefResolver
// the dispatch + ticket-reference seams ride); a nil resolver (never wired) leaves the room on the
// legacy caller-Team scope.
type ProjectTeamResolver interface {
	ResolveProjectTeam(ctx context.Context, projectID string) (teamID uuid.UUID, ok bool, err error)
}

// SetProjectTeamResolver wires the room-tenancy seam onto a handler built via NewHandler/
// NewHandlerWithDeps (post-construction, same rationale as SetMentionDispatcher / SetTicketRefResolver:
// the existing constructors — and every test riding them — stay source-compatible). Without it, a room
// is scoped to the caller's own Team exactly as before.
func (h *Handler) SetProjectTeamResolver(r ProjectTeamResolver) { h.teamResolver = r }

// roomTeam resolves the tenancy Team of the discussion room addressed by projectID, and reports whether
// the caller may operate on it. The returned Team is the fence every store call is scoped to:
//
//   - no resolver wired, or the project's Team is unknown (dev host / uncomposed squad) ⇒ fall back to
//     the caller's own Team (auth.TeamID) — the pre-ISI-5198 behaviour, so dev/test setups are unchanged;
//   - the caller is an admin, OR the caller's own Team IS the project's Team ⇒ scope to the project's
//     Team (an admin reaches any room; a project member reaches their own);
//   - a NON-admin whose Team ≠ the project's Team ⇒ ok=false (deny): the caller must not see or write
//     another squad's room, and the handler answers 404 (existence-hiding, NFR-SEC5 — indistinguishable
//     from a wholly absent project).
func (h *Handler) roomTeam(ctx context.Context, projectID string, auth AuthorContext) (uuid.UUID, bool) {
	if h.teamResolver == nil {
		return auth.TeamID, true
	}
	projTeam, ok, err := h.teamResolver.ResolveProjectTeam(ctx, projectID)
	if err != nil || !ok || projTeam == uuid.Nil {
		return auth.TeamID, true
	}
	if auth.IsAdmin || auth.TeamID == projTeam {
		return projTeam, true
	}
	return uuid.Nil, false
}

// scopedAuth resolves the caller's identity AND the room's tenancy Team in one step, so every handler
// fences on the SAME Team (the room's owning Team, ISI-5198) rather than the caller's session Team. It
// fails closed 401 when unauthenticated (defence-in-depth under the BFFAuthz choke point), and 404 —
// existence-hiding — when a non-admin addresses a room owned by another squad. On ok it returns the
// auth context (the sole provenance source) and the Team id every store call must be scoped to.
func (h *Handler) scopedAuth(w http.ResponseWriter, r *http.Request, projectID string) (AuthorContext, uuid.UUID, bool) {
	auth, ok := requireAuth(w, r)
	if !ok {
		return AuthorContext{}, uuid.Nil, false
	}
	teamID, ok := h.roomTeam(r.Context(), projectID, auth)
	if !ok {
		writeError(w, http.StatusNotFound, "not found")
		return AuthorContext{}, uuid.Nil, false
	}
	return auth, teamID, true
}
