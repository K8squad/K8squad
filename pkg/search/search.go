// Package search is the Story 8.18 global-search read model: a RBAC-scoped full-text query over the
// coordination store's work items (coord.work_item), backed by the Postgres FTS index that migration
// 0012 adds (the search_tsv generated tsvector + its GIN index).
//
// WHY WORK ITEMS ARE THE CORPUS: 8.18 asks for a "global search API … RBAC-scoped in query per
// ADR-039". The only durable, human-authored, searchable corpus in the coordination store is the work
// item (title + body). Projects/Teams/Runs are CRDs (informer cache, not rows — the overview/dashboard
// read models already project those by name), and the audit_log / discussion streams are activity, not
// searchable entities. So v1 search is work-item search. The Result envelope carries a Type field so a
// later migration can widen the corpus without breaking the wire contract; today Type is always
// "work_item".
//
// ADR-039 — SCOPE IS IN THE QUERY, NOT THE INDEX: the FTS index (0012) is tenancy-blind. Every Search
// applies the caller's authorization as a WHERE predicate alongside the @@ match:
//   - a non-admin caller is fenced to their own Team (team_id = caller's Team, §12.1 tenancy root) —
//     an item outside their Team is invisible, exactly as coord.HumanStateStore / the dashboard fence;
//   - an admin (global_role=admin, fleet-wide authority) searches every Team — AllTeams drops the
//     predicate. This mirrors the audit read model's admin/self split (ISI-2881).
//
// Keeping the scope in the query (not a per-Team index) is the ADR-039 contract: one index, the scope
// is a bound parameter the handler derives server-side from AuthorContext — never from request text.
//
// SEAM SHAPE (mirrors internal/apiserver.AuditTrailReader, ISI-2881): Searcher is the interface the
// HTTP handler rides; PostgresSearcher is the production impl over the shared *sql.DB. The one-method
// queryer indirection lets the unit lane drive the scan/rank/error paths with canned rows, while the
// SQL text itself (column order, ts_rank_cd ordering, ts_headline markup, the tenancy predicate) is
// proven against a real Postgres by the //go:build search_integration lane.
package search

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Result is one search hit — the JSON shape the console's global-search dropdown renders. Snippet is
// a server-generated fragment with <mark>…</mark> around the matched terms (Postgres ts_headline); the
// BFF/console treats it as markup carrying ONLY <mark> and sanitizes accordingly.
type Result struct {
	Type      string    `json:"type"`                // "work_item" (v1 corpus); reserved for future entity types
	ID        string    `json:"id"`                  // coord.work_item.id (uuid)
	ProjectID string    `json:"projectId,omitempty"` // owning Project (uuid); "" only if the row predates project scoping
	Title     string    `json:"title"`
	Snippet   string    `json:"snippet"` // ts_headline over title+body, <mark>…</mark> highlighted
	State     string    `json:"state"`   // board lane (backlog|todo|in_progress|in_review|done)
	Rank      float64   `json:"rank"`    // ts_rank_cd relevance (title hits weighted above body — 0012 setweight)
	UpdatedAt time.Time `json:"updatedAt"`
}

// Query is the server-side search after RBAC scoping — the handler builds it from the request + the
// caller's AuthorContext; the Searcher executes it verbatim.
type Query struct {
	// Text is the raw user query. It is parsed by websearch_to_tsquery (quoted phrases, OR, and the
	// leading-minus negation) — a forgiving grammar that NEVER errors on punctuation, so a user's
	// stray characters degrade to "no match", not a 500.
	Text string
	// TeamID is the caller's Team scope (uuid text). Applied as team_id = TeamID when AllTeams is
	// false. An empty TeamID with AllTeams=false is a fail-closed no-op: the query is fenced to a
	// Team that cannot exist, so it returns nothing (a caller with no resolved Team sees nothing).
	TeamID string
	// AllTeams drops the tenancy predicate — set ONLY for a global_role=admin caller (fleet-wide
	// authority). The handler derives this from AuthorContext.IsAdmin; it is never request-supplied.
	AllTeams bool
	// Limit bounds the page (the handler clamps 1..maxLimit, default defaultLimit).
	Limit int
}

// ErrEmptyQuery marks a blank/whitespace-only query. The handler rejects this BEFORE calling the
// store (400) so an empty tsquery never round-trips to Postgres; it is exported so the handler and
// tests share one sentinel.
var ErrEmptyQuery = errors.New("search: empty query")

const (
	defaultLimit = 20
	maxLimit     = 50
)

// Searcher answers RBAC-scoped full-text queries over the work-item corpus. Production wires the
// Postgres searcher over the shared DSN; tests wire a fake.
type Searcher interface {
	Search(ctx context.Context, q Query) ([]Result, error)
}

// rows is the slice of *sql.Rows the searcher scans — an interface so the unit lane can drive the
// scan/error paths with canned rows without a live database.
type rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
	Close() error
}

// queryer executes one bounded query. *sql.DB satisfies it via the dbQueryer wrapper, so the prod
// adapter is a one-method shim; unit tests inject a fake.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (rows, error)
}

type dbQueryer struct{ db *sql.DB }

func (d dbQueryer) QueryContext(ctx context.Context, query string, args ...any) (rows, error) {
	return d.db.QueryContext(ctx, query, args...)
}

// PostgresSearcher is the production Searcher over coord.work_item + the 0012 FTS index. Read-only;
// every value (query text, team scope, limit) is a bound parameter — the SQL text is static and the
// only structural variation is the presence of the tenancy predicate (AllTeams), so no request text
// is ever interpolated.
type PostgresSearcher struct {
	q queryer
}

// NewPostgresSearcher builds the production searcher over db.
func NewPostgresSearcher(db *sql.DB) (*PostgresSearcher, error) {
	if db == nil {
		return nil, errors.New("search.NewPostgresSearcher: nil db")
	}
	return &PostgresSearcher{q: dbQueryer{db: db}}, nil
}

// ts_headline options: highlight matched terms with <mark>…</mark> (the ONLY markup the console
// renders, after sanitizing), a single short fragment suitable for a dropdown row.
const headlineOpts = `StartSel=<mark>, StopSel=</mark>, MaxWords=18, MinWords=5, ShortWord=2, MaxFragments=1, HighlightAll=FALSE`

// Search runs the FTS query with the caller's RBAC scope applied in-query (ADR-039). Ordering is
// relevance-first (ts_rank_cd DESC — title hits outrank body hits via the 0012 setweight A/B), then
// most-recently-updated as a stable tiebreak. A blank query is a caller error (ErrEmptyQuery); the
// handler guards it, but the store double-checks so a direct caller cannot issue an empty tsquery.
func (s *PostgresSearcher) Search(ctx context.Context, q Query) ([]Result, error) {
	if strings.TrimSpace(q.Text) == "" {
		return nil, ErrEmptyQuery
	}
	// Fail-closed on a caller with no resolved Team: a non-admin whose TeamID is empty is fenced to
	// a Team that cannot exist, so the honest answer is no rows. We short-circuit here rather than
	// bind "" as a uuid (which Postgres rejects at parse time, 22P02) — the deny outcome must never
	// depend on a DB error path.
	if !q.AllTeams && q.TeamID == "" {
		return []Result{}, nil
	}
	limit := q.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	if limit > maxLimit {
		limit = maxLimit
	}

	// $1 = query text (→ websearch_to_tsquery), $2 = headline options. The tenancy predicate and
	// the LIMIT append their own bound params so nothing is string-interpolated from the request.
	args := []any{q.Text, headlineOpts}
	var scope string
	if !q.AllTeams {
		// Fenced to the caller's Team. An empty TeamID here fences to a non-existent Team → no rows
		// (fail-closed), which is the correct answer for a caller whose Team did not resolve.
		args = append(args, q.TeamID)
		scope = fmt.Sprintf("AND w.team_id = $%d::uuid", len(args))
	}
	args = append(args, limit)
	limitArg := len(args)

	// websearch_to_tsquery is computed ONCE in a CTE so the @@ match, the rank, and the headline all
	// reference the same parsed query (and Postgres does not re-parse it three times).
	query := fmt.Sprintf(`
		WITH q AS (SELECT websearch_to_tsquery('english', $1) AS tsq)
		SELECT w.id::text,
		       COALESCE(w.project_id::text, ''),
		       w.title,
		       ts_headline('english', COALESCE(w.title,'') || ' — ' || COALESCE(w.body,''), q.tsq, $2),
		       w.state,
		       ts_rank_cd(w.search_tsv, q.tsq),
		       w.updated_at
		  FROM coord.work_item w, q
		 WHERE w.search_tsv @@ q.tsq
		   %s
		 ORDER BY ts_rank_cd(w.search_tsv, q.tsq) DESC, w.updated_at DESC
		 LIMIT $%d`, scope, limitArg)

	r, err := s.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("search: query: %w", err)
	}
	defer r.Close()

	out := make([]Result, 0, limit)
	for r.Next() {
		var res Result
		res.Type = "work_item"
		if err := r.Scan(&res.ID, &res.ProjectID, &res.Title, &res.Snippet, &res.State, &res.Rank, &res.UpdatedAt); err != nil {
			return nil, fmt.Errorf("search: scan: %w", err)
		}
		out = append(out, res)
	}
	if err := r.Err(); err != nil {
		return nil, fmt.Errorf("search: rows: %w", err)
	}
	return out, nil
}
