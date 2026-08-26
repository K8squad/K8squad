package runsource

import (
	"context"
	"errors"
	"testing"

	"github.com/K8squad/K8squad/internal/buildbrowser"
)

// fakeSnapshotStore is an in-memory SnapshotStore for the reader unit tests.
type fakeSnapshotStore struct {
	rows map[string]SnapshotRow
	err  error
}

func (f *fakeSnapshotStore) SnapshotByRun(_ context.Context, runID string) (SnapshotRow, bool, error) {
	if f.err != nil {
		return SnapshotRow{}, false, f.err
	}
	row, ok := f.rows[runID]
	return row, ok, nil
}

// fakeBundles serves fixed bundle bytes by uri (or an error).
type fakeBundles struct {
	bytes map[string][]byte
	err   error
}

func (f *fakeBundles) Bundle(_ context.Context, uri, _ string) ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.bytes[uri], nil
}

func run(id string) buildbrowser.RunMeta { return buildbrowser.RunMeta{RunID: id} }

// TestMeta_FromSummary proves the meta route serves from the persisted 8.7c summary WITHOUT hydrating
// a bundle, and carries the live:false contract — the real production read with no KSQUAD_DEV_RUNS.
func TestMeta_FromSummary(t *testing.T) {
	store := &fakeSnapshotStore{rows: map[string]SnapshotRow{
		"r1": {URI: "sha256:abc", SHA256: "abc", Summary: buildbrowser.SnapshotSummary{
			Base: "base12", RunRef: "run/r1", Commit: "head34", FileCount: 7,
		}},
	}}
	r := NewSnapshotStoreReader(store, nil)
	got, err := r.Meta(context.Background(), run("r1"))
	if err != nil {
		t.Fatalf("Meta: %v", err)
	}
	if got.RunID != "r1" || got.Head != "head34" || got.Base != "base12" || got.ChangedFiles != 7 {
		t.Fatalf("Meta = %+v", got)
	}
	if got.Live {
		t.Fatalf("Meta.Live = true, want false (completed-Run snapshot)")
	}
}

// TestMeta_UnknownRun_NotFound proves an unknown Run is ErrNotFound (existence-hiding), never a
// distinct signal a neighbour could probe.
func TestMeta_UnknownRun_NotFound(t *testing.T) {
	r := NewSnapshotStoreReader(&fakeSnapshotStore{rows: map[string]SnapshotRow{}}, nil)
	if _, err := r.Meta(context.Background(), run("nope")); !errors.Is(err, buildbrowser.ErrNotFound) {
		t.Fatalf("Meta err = %v, want ErrNotFound", err)
	}
}

// TestByteReads_DegradeWithoutResolver proves that without a wired BundleResolver (v1, pre-ISI-2900)
// tree/diff/file degrade to ErrNotFound — the same 404 a missing Run returns — so the missing blob
// store is a legible existence-hiding degradation, not a 500 or a partial read.
func TestByteReads_DegradeWithoutResolver(t *testing.T) {
	store := &fakeSnapshotStore{rows: map[string]SnapshotRow{
		"r1": {URI: "sha256:abc", Summary: buildbrowser.SnapshotSummary{Base: "b", Commit: "c", FileCount: 1}},
	}}
	r := NewSnapshotStoreReader(store, nil) // no resolver

	if _, err := r.Tree(context.Background(), run("r1"), "run"); !errors.Is(err, buildbrowser.ErrNotFound) {
		t.Fatalf("Tree err = %v, want ErrNotFound", err)
	}
	if _, err := r.Diff(context.Background(), run("r1")); !errors.Is(err, buildbrowser.ErrNotFound) {
		t.Fatalf("Diff err = %v, want ErrNotFound", err)
	}
	if _, err := r.File(context.Background(), run("r1"), "run", "main.go"); !errors.Is(err, buildbrowser.ErrNotFound) {
		t.Fatalf("File err = %v, want ErrNotFound", err)
	}
}

// TestByteReads_TruncatedSnapshot_NotFound proves a truncated (byte-less) capture serves Meta but has
// no servable bytes — byte reads are ErrNotFound even with a resolver wired.
func TestByteReads_TruncatedSnapshot_NotFound(t *testing.T) {
	store := &fakeSnapshotStore{rows: map[string]SnapshotRow{
		"r1": {URI: "", SHA256: "", Summary: buildbrowser.SnapshotSummary{Base: "b", Commit: "c", FileCount: 9, Truncated: true}},
	}}
	r := NewSnapshotStoreReader(store, &fakeBundles{bytes: map[string][]byte{}})

	// Meta still serves the summary of a truncated capture.
	if m, err := r.Meta(context.Background(), run("r1")); err != nil || m.ChangedFiles != 9 {
		t.Fatalf("Meta = (%+v, %v), want ChangedFiles=9", m, err)
	}
	if _, err := r.Diff(context.Background(), run("r1")); !errors.Is(err, buildbrowser.ErrNotFound) {
		t.Fatalf("Diff on truncated err = %v, want ErrNotFound", err)
	}
}

// TestByteReads_ResolverMiss_NotFound proves a resolver that cannot produce bytes (miss or error)
// collapses to ErrNotFound rather than a 500 — existence-hiding on the byte path too.
func TestByteReads_ResolverMiss_NotFound(t *testing.T) {
	store := &fakeSnapshotStore{rows: map[string]SnapshotRow{
		"r1": {URI: "sha256:abc", SHA256: "abc", Summary: buildbrowser.SnapshotSummary{Base: "b", Commit: "c"}},
	}}
	// resolver returns nil bytes for the uri (a miss) — not-found, not an error surfaced upward.
	r := NewSnapshotStoreReader(store, &fakeBundles{bytes: map[string][]byte{}})
	if _, err := r.Tree(context.Background(), run("r1"), "run"); !errors.Is(err, buildbrowser.ErrNotFound) {
		t.Fatalf("Tree resolver-miss err = %v, want ErrNotFound", err)
	}

	// resolver ERROR is likewise mapped to not-found (never leaks whether the Run exists).
	r2 := NewSnapshotStoreReader(store, &fakeBundles{err: errors.New("blob store down")})
	if _, err := r2.Tree(context.Background(), run("r1"), "run"); !errors.Is(err, buildbrowser.ErrNotFound) {
		t.Fatalf("Tree resolver-error err = %v, want ErrNotFound", err)
	}
}

// TestStoreError_Propagates proves a genuine store failure (not a miss) is surfaced, not swallowed as
// not-found — the reader hides existence, not infrastructure faults, on the meta path.
func TestStoreError_Propagates(t *testing.T) {
	r := NewSnapshotStoreReader(&fakeSnapshotStore{err: errors.New("db down")}, nil)
	if _, err := r.Meta(context.Background(), run("r1")); err == nil || errors.Is(err, buildbrowser.ErrNotFound) {
		t.Fatalf("Meta err = %v, want a wrapped store error (not ErrNotFound)", err)
	}
}
