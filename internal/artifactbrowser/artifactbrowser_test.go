package artifactbrowser

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/K8squad/K8squad/internal/buildbrowser"
	"github.com/K8squad/K8squad/pkg/coord"
)

// fakeStore is the in-memory Store seam: rows keyed by run, content keyed by uri.
type fakeStore struct {
	rows    map[string][]Artifact
	content map[string][]byte
	// failContent, when set, makes every Content call fail (unresolvable uri).
	failContent bool
	// listCalls counts ListByRun invocations (targeted-read discipline check).
	listCalls int
}

func (f *fakeStore) ListByRun(_ context.Context, runID string) ([]Artifact, error) {
	f.listCalls++
	return f.rows[runID], nil
}

func (f *fakeStore) GetByRunAndID(_ context.Context, runID, artifactID string) (Artifact, bool, error) {
	for _, a := range f.rows[runID] {
		if a.ID == artifactID {
			return a, true, nil
		}
	}
	return Artifact{}, false, nil
}

func (f *fakeStore) Content(_ context.Context, a Artifact) ([]byte, error) {
	if f.failContent {
		return nil, errors.New("unresolvable")
	}
	raw, ok := f.content[a.URI]
	if !ok {
		return nil, errors.New("no bytes at uri")
	}
	return raw, nil
}

func runID() string { return uuid.New().String() }

// newFixture wires a service with one Run owned by alice in teamA, plus bob (same Team non-owner)
// and carol (other Team). Mirrors the build-browser gate tests so both read models stay aligned.
func newFixture(t *testing.T) (svc *Service, owner, peer, foreign buildbrowser.Caller, rid string) {
	t.Helper()
	teamA, teamB := uuid.New(), uuid.New()
	rid = runID()
	src := buildbrowser.NewStaticRunSource(map[string]buildbrowser.RunMeta{
		rid: {RunID: rid, TeamID: teamA, Principal: "user:alice"},
	})
	store := &fakeStore{rows: map[string][]Artifact{}, content: map[string][]byte{}}
	return NewService(src, store),
		buildbrowser.Caller{Principal: "user:alice", TeamID: teamA},
		buildbrowser.Caller{Principal: "user:bob", TeamID: teamA},
		buildbrowser.Caller{Principal: "user:carol", TeamID: teamB},
		rid
}

func handoffArtifact(rid string) string {
	return coord.AuditHandoffURI + uuid.New().String()
}

func handoffRow(rid, uri string) Artifact {
	return Artifact{
		ID: uuid.New().String(), WorkItemID: uuid.New().String(), RunID: rid,
		Kind: coord.HandoffKind, URI: uri,
		SHA256: "deadbeef", CreatedAt: time.Now(),
	}
}

func TestListing_OwnerSeesArtifactsAndHandoff(t *testing.T) {
	svc, owner, _, _, rid := newFixture(t)
	uri := handoffArtifact(rid)
	h := handoffRow(rid, uri)
	svc.Store.(*fakeStore).rows[rid] = []Artifact{h}
	doc := coord.HandoffDoc{Did: []string{"shipped"}, Findings: "f", Next: []string{"n"}}
	raw, _ := json.Marshal(doc)
	svc.Store.(*fakeStore).content[uri] = raw

	l, err := svc.Listing(context.Background(), owner, rid)
	if err != nil {
		t.Fatalf("Listing: %v", err)
	}
	if len(l.Artifacts) != 1 || l.Artifacts[0].Kind != coord.HandoffKind {
		t.Fatalf("artifacts = %+v", l.Artifacts)
	}
	if l.Handoff == nil || len(l.Handoff.Did) != 1 || l.Handoff.Did[0] != "shipped" {
		t.Fatalf("handoff = %+v", l.Handoff)
	}
}

func TestListing_MalformedHandoffDegradesToRow(t *testing.T) {
	svc, owner, _, _, rid := newFixture(t)
	uri := handoffArtifact(rid)
	h := handoffRow(rid, uri)
	svc.Store.(*fakeStore).rows[rid] = []Artifact{h}
	svc.Store.(*fakeStore).content[uri] = []byte("not json")

	l, err := svc.Listing(context.Background(), owner, rid)
	if err != nil {
		t.Fatalf("Listing must not fail on a malformed handoff: %v", err)
	}
	if l.Handoff != nil {
		t.Fatalf("handoff = %+v, want nil", l.Handoff)
	}
	if len(l.Artifacts) != 1 {
		t.Fatalf("artifacts = %+v", l.Artifacts)
	}
}

// TestGate_ExistenceHiding — the ONE contract: cross-Team, same-Team non-owner, and a missing Run
// all return ErrNotFound, indistinguishable from each other (NFR-SEC5).
func TestGate_ExistenceHiding(t *testing.T) {
	svc, _, peer, foreign, rid := newFixture(t)
	svc.Store.(*fakeStore).rows[rid] = []Artifact{handoffRow(rid, handoffArtifact(rid))}

	for name, c := range map[string]buildbrowser.Caller{
		"same-team non-owner": peer,
		"cross-team":          foreign,
		"nil team":            {Principal: "user:alice"},
	} {
		if _, err := svc.Listing(context.Background(), c, rid); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: Listing err = %v, want ErrNotFound", name, err)
		}
		if _, err := svc.Content(context.Background(), c, rid, "x"); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: Content err = %v, want ErrNotFound", name, err)
		}
	}
	if _, err := svc.Listing(context.Background(), buildbrowser.Caller{Principal: "user:alice"}, "no-such-run"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing run: err = %v, want ErrNotFound", err)
	}
}

// TestContent_ResolvedWithinGatedRun — a guessed artifact id from ANOTHER run's rows must not
// resolve even for an otherwise-authorized caller of this Run.
func TestContent_ResolvedWithinGatedRun(t *testing.T) {
	svc, owner, _, _, rid := newFixture(t)
	mine := handoffRow(rid, handoffArtifact(rid))
	foreignArt := handoffRow(runID(), handoffArtifact(runID()))
	svc.Store.(*fakeStore).rows[rid] = []Artifact{mine}
	svc.Store.(*fakeStore).rows[foreignArt.RunID] = []Artifact{foreignArt}
	svc.Store.(*fakeStore).content[mine.URI] = []byte(`{"did":["x"]}`)
	svc.Store.(*fakeStore).content[foreignArt.URI] = []byte(`{"did":["y"]}`)

	if _, err := svc.Content(context.Background(), owner, rid, foreignArt.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-run artifact id: err = %v, want ErrNotFound", err)
	}
	res, err := svc.Content(context.Background(), owner, rid, mine.ID)
	if err != nil {
		t.Fatalf("Content: %v", err)
	}
	if string(res.Content) != `{"did":["x"]}` || res.Truncated || res.Size != len(res.Content) {
		t.Fatalf("res = %+v", res)
	}
	if _, err := svc.Content(context.Background(), owner, rid, ""); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("empty id: err = %v, want ErrBadRequest", err)
	}
}

// TestContent_Truncation — a blob over MaxArtifactBytes comes back capped, flagged, with the FULL
// size reported.
func TestContent_Truncation(t *testing.T) {
	svc, owner, _, _, rid := newFixture(t)
	h := handoffRow(rid, handoffArtifact(rid))
	svc.Store.(*fakeStore).rows[rid] = []Artifact{h}
	svc.Store.(*fakeStore).content[h.URI] = []byte(strings.Repeat("a", MaxArtifactBytes+1))

	res, err := svc.Content(context.Background(), owner, rid, h.ID)
	if err != nil {
		t.Fatalf("Content: %v", err)
	}
	if !res.Truncated || len(res.Content) != MaxArtifactBytes || res.Size != MaxArtifactBytes+1 {
		t.Fatalf("truncation contract broken: truncated=%v len=%d size=%d", res.Truncated, len(res.Content), res.Size)
	}
}

// TestListing_NonUUIDRunHasNoRows — a run id that cannot be a coord uuid short-circuits to
// ErrNotFound without touching the store.
func TestListing_NonUUIDRunHasNoRows(t *testing.T) {
	svc, owner, _, _, _ := newFixture(t)
	// A run whose RunMeta carries a non-uuid RunID (a dev/static source row).
	teamA := owner.TeamID
	src := buildbrowser.NewStaticRunSource(map[string]buildbrowser.RunMeta{
		"dev-run": {RunID: "dev-run", TeamID: teamA, Principal: owner.Principal},
	})
	svc.Runs = src
	if _, err := svc.Listing(context.Background(), owner, "dev-run"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// TestContent_NonUUIDIdsMatchListingAnswer — cursor review: Content must apply the SAME
// non-uuid short-circuit Listing does, so a dev/static Run (or a non-uuid artifact id) gets
// ErrNotFound→404 on BOTH routes instead of a uuid-cast 500 on the content route.
func TestContent_NonUUIDIdsMatchListingAnswer(t *testing.T) {
	svc, owner, _, _, _ := newFixture(t)
	teamA := owner.TeamID
	src := buildbrowser.NewStaticRunSource(map[string]buildbrowser.RunMeta{
		"dev-run": {RunID: "dev-run", TeamID: teamA, Principal: owner.Principal},
	})
	svc.Runs = src
	if _, err := svc.Content(context.Background(), owner, "dev-run", uuid.New().String()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("non-uuid run: err = %v, want ErrNotFound", err)
	}
	if _, err := svc.Content(context.Background(), owner, "dev-run", "not-a-uuid"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("non-uuid artifact id: err = %v, want ErrNotFound", err)
	}
}

// TestContent_TargetedReadNotListScan — cursor review: Content must locate the row through
// GetByRunAndID (a single targeted read), not by pulling the run's whole list. The fake counts
// ListByRun calls; a Content hit must leave it at zero.
func TestContent_TargetedReadNotListScan(t *testing.T) {
	svc, owner, _, _, rid := newFixture(t)
	h := handoffRow(rid, handoffArtifact(rid))
	store := svc.Store.(*fakeStore)
	store.rows[rid] = []Artifact{h}
	store.content[h.URI] = []byte(`{"did":["x"]}`)
	store.listCalls = 0

	if _, err := svc.Content(context.Background(), owner, rid, h.ID); err != nil {
		t.Fatalf("Content: %v", err)
	}
	if store.listCalls != 0 {
		t.Fatalf("Content used ListByRun (%d calls) instead of a targeted GetByRunAndID", store.listCalls)
	}
}
