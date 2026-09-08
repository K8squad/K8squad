package apiserver

import (
	"context"
	"net/http"
	"sort"

	"sigs.k8s.io/controller-runtime/pkg/client"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/internal/discussion"
)

// ============================================================================
// Teams LIST read model (ISI-3953, gap G4 of the ISI-3949 fleet-admin audit) —
// GET /api/teams enumerates the Teams the caller may see.
// ============================================================================
//
// Before this file the only GET under /api/teams/* was the per-team org detail
// (/api/teams/{teamId}/org), so an admin could open a Team only by ALREADY
// knowing its UID — there was no way to discover the fleet's Teams. /api/teams
// itself was POST-only (the 8.5 compose collection). This is the missing LIST.
//
// It extends the ADR-039 / ADR-0010 admin-fleet-wide read-model family exactly
// as search.go / overview.go do: a global admin (AuthorContext.IsAdmin) gets
// EVERY Team from the shared informer cache; a tenant gets ONLY their own Team,
// resolved by UID (AuthorContext.TeamID) — never a hint that other Teams exist
// (existence-hiding, §12.1). Source of truth is the SAME controller-runtime
// cache that backs overview/org/credentials: no new watch, no second copy, no
// SQL. It is the enumeration source the fleet Team picker (ISI-3950, gap G1)
// consumes to feed /api/teams/{uid}/org.

// TeamsList is the GET /api/teams payload: the Teams the caller may see. Fleet
// is set only for a global-admin caller (the list then spans every squad); a
// tenant caller leaves it false and sees exactly their own Team. Each entry is
// a TeamRef (name/namespace/uid) — the uid is the object UID the console feeds
// straight back into GET /api/teams/{uid}/org.
type TeamsList struct {
	Teams []TeamRef `json:"teams"`
	Fleet bool      `json:"fleet,omitempty"`
}

// TeamsReader lists the Teams a caller may see. admin ⇒ fleet-wide (every Team,
// ADR-039 / ISI-3932); non-admin ⇒ exactly the Team whose UID is callerTeamUID
// (existence-hiding — a dangling binding yields an empty list, never an error
// that would betray whether other Teams exist). It is the seam the handler
// rides: production wires the cache-backed reader; tests wire a fake
// client.Reader. A reader MUST NOT leak another Team to a non-admin caller.
type TeamsReader interface {
	Teams(ctx context.Context, callerTeamUID string, admin bool) (TeamsList, error)
}

// ClientTeamsReader is the production TeamsReader over any client.Reader — in
// the host that is the controller-runtime cache (informer-backed, in-memory);
// in tests it is a fake client seeded with objects. It performs no writes.
type ClientTeamsReader struct {
	reader client.Reader
}

// NewClientTeamsReader builds the read model over a client.Reader (the informer
// cache in the host). The reader's scheme must have api/v1alpha1 registered
// (see NewCacheReader).
func NewClientTeamsReader(r client.Reader) *ClientTeamsReader {
	return &ClientTeamsReader{reader: r}
}

// Teams lists the visible Teams. For an admin it returns every Team in the
// cluster (Fleet=true); for a tenant it returns only the Team whose object UID
// matches callerTeamUID. Listing cluster-wide and filtering to one UID mirrors
// resolveTeam/Overview (org.go, overview.go): the session's Team scope is a UID
// (not a name), so a rename can never widen the scope and a name collision
// across namespaces can never cross tenancy. Output is deterministic (sorted by
// namespace, then name) so the console renders a stable order.
func (r *ClientTeamsReader) Teams(ctx context.Context, callerTeamUID string, admin bool) (TeamsList, error) {
	var teams ksquadv1.TeamList
	if err := r.reader.List(ctx, &teams); err != nil { // no InNamespace ⇒ every squad
		return TeamsList{}, err
	}

	out := TeamsList{Teams: []TeamRef{}, Fleet: admin}
	for i := range teams.Items {
		t := &teams.Items[i]
		if !admin {
			// Existence-hiding: a non-admin only ever sees the single Team their
			// session resolves to. An empty callerTeamUID (or a UID matching no
			// Team CR — e.g. a dangling binding) yields an empty list, never an
			// error, so the route cannot betray whether other Teams exist.
			if callerTeamUID == "" || string(t.UID) != callerTeamUID {
				continue
			}
		}
		out.Teams = append(out.Teams, TeamRef{
			Name:      t.Name,
			Namespace: t.Namespace,
			UID:       string(t.UID),
		})
	}
	sort.Slice(out.Teams, func(a, b int) bool {
		if out.Teams[a].Namespace != out.Teams[b].Namespace {
			return out.Teams[a].Namespace < out.Teams[b].Namespace
		}
		return out.Teams[a].Name < out.Teams[b].Name
	})
	return out, nil
}

// teams is the handler behind GET /api/teams. It rides the §13 BFF authz choke
// point (mounted in routes), so the AuthorContext is already stamped on the
// request context; NOTHING is read from the request body/query. A caller with
// no resolved scope gets 401 (defence in depth over BFFAuthz); a reader error
// is 502. Unlike the org detail route there is no 404 — an admin always gets a
// (possibly empty) fleet list and a tenant always gets their own (possibly
// empty) single-element list, so absence is a normal empty payload, not a 404
// that could leak existence.
func (s *Server) teams(reader TeamsReader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth, ok := discussion.AuthFromContext(r.Context())
		if !ok || auth.Principal == "" {
			writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
			return
		}
		list, err := reader.Teams(r.Context(), auth.TeamID.String(), auth.IsAdmin)
		if err != nil {
			writeJSONError(w, http.StatusBadGateway, "teams read model unavailable")
			return
		}
		writeJSON(w, http.StatusOK, list)
	}
}
