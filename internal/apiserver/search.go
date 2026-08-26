package apiserver

// ============================================================================
// Global search API (story 8.18 / ISI-2912, gap ISI-2876) — the read side of
// the coord.work_item full-text index (migration 0012).
//
//   GET /api/search?q=<text>&limit=<n>
//
// Mounted behind the §13 BFFAuthz choke point (identity is the server-derived
// AuthorContext, never a header). RBAC scoping is deny-by-default (§12.3) and,
// per ADR-039, applied IN THE QUERY by pkg/search:
//
//   - admin (global_role=admin): fleet-wide — searches every Team's work items;
//   - non-admin: fenced to the caller's Team (AuthorContext.TeamID, §12.1) — an
//     item outside their Team is invisible (never surfaced, never counted),
//     exactly as the dashboard / audit surfaces fence tenancy.
//
// The query text is a bound parameter parsed by Postgres websearch_to_tsquery
// (pkg/search) — the handler never interpolates it into SQL and never widens
// the scope from request input. A blank q is a 400 (an empty tsquery is not a
// search); an unwired searcher keeps the route's documented 501 (DB-less dev
// run), exactly like the other read models.
// ============================================================================

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/K8squad/K8squad/pkg/search"
)

// searchDefaultLimit / searchMaxLimit bound the top-bar dropdown page. pkg/search
// clamps again defensively; the handler clamps here so the wire contract is
// explicit and a hostile ?limit cannot ask Postgres for an unbounded scan.
const (
	searchDefaultLimit = 20
	searchMaxLimit     = 50
)

// SearchResponse is the GET /api/search payload: the echoed query plus the
// relevance-ordered hits. results is always a JSON array (never null) so the
// console can render an empty state without a nil guard.
type SearchResponse struct {
	Query   string          `json:"query"`
	Results []search.Result `json:"results"`
}

// searchHandler answers GET /api/search. Rides the §13 choke point (mounted in
// routes); derives the RBAC scope from AuthorContext; parses/validates q+limit.
func searchHandler(searcher search.Searcher) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		author, ok := discussion.AuthFromContext(r.Context())
		if !ok || author.Principal == "" {
			// Defence in depth: BFFAuthz already guarantees this.
			writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
			return
		}

		text := r.URL.Query().Get("q")
		if text == "" {
			writeJSONError(w, http.StatusBadRequest, "q (search text) required")
			return
		}

		q := search.Query{
			Text:     text,
			Limit:    searchLimitParam(r),
			AllTeams: author.IsAdmin,         // admin: fleet-wide (ADR-039)
			TeamID:   author.TeamID.String(), // non-admin: fenced to this Team (§12.1)
		}

		results, err := searcher.Search(r.Context(), q)
		switch {
		case errors.Is(err, search.ErrEmptyQuery):
			// A query of only stopwords/punctuation still parses to empty — treat as a bad request,
			// not a 500 (the guard above catches the literal-empty case first).
			writeJSONError(w, http.StatusBadRequest, "q (search text) required")
			return
		case err != nil:
			writeJSONError(w, http.StatusBadGateway, "search unavailable")
			return
		}

		writeJSON(w, http.StatusOK, SearchResponse{Query: text, Results: results})
	}
}

// searchLimitParam clamps ?limit into the 1..searchMaxLimit window (default
// searchDefaultLimit). A missing/garbage/out-of-range value falls back to the
// default rather than erroring — the top-bar never needs to hand-tune it.
func searchLimitParam(r *http.Request) int {
	v := r.URL.Query().Get("limit")
	if v == "" {
		return searchDefaultLimit
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return searchDefaultLimit
	}
	if n > searchMaxLimit {
		return searchMaxLimit
	}
	return n
}
