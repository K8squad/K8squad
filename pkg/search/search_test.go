package search

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeRows is a canned result set for driving the scan/iteration paths without Postgres.
type fakeRows struct {
	data    [][]any
	i       int
	scanErr error
	iterErr error
	closed  bool
}

func (f *fakeRows) Next() bool { return f.i < len(f.data) }
func (f *fakeRows) Scan(dest ...any) error {
	if f.scanErr != nil {
		return f.scanErr
	}
	row := f.data[f.i]
	f.i++
	for i := range dest {
		switch d := dest[i].(type) {
		case *string:
			*d = row[i].(string)
		case *float64:
			*d = row[i].(float64)
		case *time.Time:
			*d = row[i].(time.Time)
		default:
			return errors.New("fakeRows: unexpected dest type")
		}
	}
	return nil
}
func (f *fakeRows) Err() error   { return f.iterErr }
func (f *fakeRows) Close() error { f.closed = true; return nil }

// fakeQueryer captures the SQL + args and returns a canned rows/err.
type fakeQueryer struct {
	gotQuery string
	gotArgs  []any
	rows     *fakeRows
	err      error
}

func (f *fakeQueryer) QueryContext(_ context.Context, query string, args ...any) (rows, error) {
	f.gotQuery = query
	f.gotArgs = args
	if f.err != nil {
		return nil, f.err
	}
	return f.rows, nil
}

func newSearcher(q *fakeQueryer) *PostgresSearcher { return &PostgresSearcher{q: q} }

func TestSearch_EmptyQuery(t *testing.T) {
	for _, text := range []string{"", "   ", "\t\n"} {
		fq := &fakeQueryer{rows: &fakeRows{}}
		_, err := newSearcher(fq).Search(context.Background(), Query{Text: text, AllTeams: true})
		if !errors.Is(err, ErrEmptyQuery) {
			t.Fatalf("text %q: expected ErrEmptyQuery, got %v", text, err)
		}
		if fq.gotQuery != "" {
			t.Fatalf("text %q: an empty query must not reach the store", text)
		}
	}
}

func TestSearch_ScopedToTeam(t *testing.T) {
	fq := &fakeQueryer{rows: &fakeRows{}}
	_, err := newSearcher(fq).Search(context.Background(), Query{
		Text: "checkout", TeamID: "team-uuid", AllTeams: false, Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fq.gotQuery, "w.team_id = $3::uuid") {
		t.Fatalf("non-admin query must carry the tenancy predicate; got:\n%s", fq.gotQuery)
	}
	// args: [text, headlineOpts, teamID, limit]
	if len(fq.gotArgs) != 4 {
		t.Fatalf("expected 4 args, got %d: %v", len(fq.gotArgs), fq.gotArgs)
	}
	if fq.gotArgs[0] != "checkout" || fq.gotArgs[2] != "team-uuid" || fq.gotArgs[3] != 10 {
		t.Fatalf("unexpected args: %v", fq.gotArgs)
	}
}

func TestSearch_AdminDropsTenancyPredicate(t *testing.T) {
	fq := &fakeQueryer{rows: &fakeRows{}}
	_, err := newSearcher(fq).Search(context.Background(), Query{
		Text: "checkout", TeamID: "ignored", AllTeams: true, Limit: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(fq.gotQuery, "team_id") {
		t.Fatalf("admin (AllTeams) query must NOT fence by team; got:\n%s", fq.gotQuery)
	}
	// args: [text, headlineOpts, limit] — no team arg.
	if len(fq.gotArgs) != 3 {
		t.Fatalf("expected 3 args for admin, got %d: %v", len(fq.gotArgs), fq.gotArgs)
	}
	if fq.gotArgs[2] != 5 {
		t.Fatalf("expected limit 5 as 3rd arg, got %v", fq.gotArgs[2])
	}
}

func TestSearch_LimitClamped(t *testing.T) {
	cases := []struct{ in, want int }{
		{in: 0, want: defaultLimit},
		{in: -3, want: defaultLimit},
		{in: 999, want: maxLimit},
		{in: 12, want: 12},
	}
	for _, c := range cases {
		fq := &fakeQueryer{rows: &fakeRows{}}
		if _, err := newSearcher(fq).Search(context.Background(), Query{Text: "x", AllTeams: true, Limit: c.in}); err != nil {
			t.Fatal(err)
		}
		got := fq.gotArgs[len(fq.gotArgs)-1].(int)
		if got != c.want {
			t.Fatalf("limit %d: clamped to %d, want %d", c.in, got, c.want)
		}
	}
}

func TestSearch_ScansResultsAndTypeIsWorkItem(t *testing.T) {
	now := time.Now().UTC()
	fq := &fakeQueryer{rows: &fakeRows{data: [][]any{
		{"id-1", "proj-1", "Fix checkout", "hit <mark>checkout</mark>", "in_progress", 0.9, now},
		{"id-2", "", "Refund flow", "the <mark>refund</mark>", "todo", 0.4, now},
	}}}
	got, err := newSearcher(fq).Search(context.Background(), Query{Text: "checkout", AllTeams: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 results, got %d", len(got))
	}
	if got[0].Type != "work_item" || got[1].Type != "work_item" {
		t.Fatalf("every v1 result Type must be work_item: %+v", got)
	}
	if got[0].ID != "id-1" || got[0].ProjectID != "proj-1" || got[0].Title != "Fix checkout" ||
		got[0].Snippet != "hit <mark>checkout</mark>" || got[0].State != "in_progress" || got[0].Rank != 0.9 {
		t.Fatalf("row 0 mis-scanned: %+v", got[0])
	}
	if got[1].ProjectID != "" {
		t.Fatalf("empty project_id must map to empty string, got %q", got[1].ProjectID)
	}
	if !fq.rows.closed {
		t.Fatal("rows must be Closed")
	}
}

func TestSearch_QueryErrorPropagates(t *testing.T) {
	fq := &fakeQueryer{err: errors.New("boom")}
	if _, err := newSearcher(fq).Search(context.Background(), Query{Text: "x", AllTeams: true}); err == nil {
		t.Fatal("expected query error to propagate")
	}
}

func TestSearch_ScanAndIterErrorsPropagate(t *testing.T) {
	scanFail := &fakeQueryer{rows: &fakeRows{data: [][]any{{"a", "b", "c", "d", "e", 0.1, time.Now()}}, scanErr: errors.New("scan")}}
	if _, err := newSearcher(scanFail).Search(context.Background(), Query{Text: "x", AllTeams: true}); err == nil {
		t.Fatal("expected scan error to propagate")
	}
	iterFail := &fakeQueryer{rows: &fakeRows{iterErr: errors.New("iter")}}
	if _, err := newSearcher(iterFail).Search(context.Background(), Query{Text: "x", AllTeams: true}); err == nil {
		t.Fatal("expected rows.Err to propagate")
	}
}

func TestNewPostgresSearcher_NilDB(t *testing.T) {
	if _, err := NewPostgresSearcher(nil); err == nil {
		t.Fatal("expected error on nil db")
	}
}
