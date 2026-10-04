package apiserver

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/K8squad/K8squad/internal/buildbrowser/readerpod"
	"github.com/K8squad/K8squad/internal/buildbrowser/readerpod/readserver"
	"github.com/K8squad/K8squad/internal/discussion"
)

// --- fakes -------------------------------------------------------------------

type fakeLauncher struct {
	mu        sync.Mutex
	launches  int
	teardowns int
	lastSpec  readerpod.Spec
	launchErr error
}

func (f *fakeLauncher) Launch(_ context.Context, spec readerpod.Spec) (readerpod.Handle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.launchErr != nil {
		return readerpod.Handle{}, f.launchErr
	}
	f.launches++
	f.lastSpec = spec
	return readerpod.Handle{
		PodName:     readerpod.PodName(spec.RunID),
		ServiceName: readerpod.ServiceName(spec.RunID),
		Namespace:   "default",
	}, nil
}

func (f *fakeLauncher) TearDown(_ context.Context, _ readerpod.Handle) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.teardowns++
	return nil
}

type fakeResolver struct {
	spec readerpod.Spec
	err  error
	seen []string
}

func (r *fakeResolver) ResolveReaderSpec(_ context.Context, projectID string) (readerpod.Spec, error) {
	r.seen = append(r.seen, projectID)
	if r.err != nil {
		return readerpod.Spec{}, r.err
	}
	return r.spec, nil
}

type fakeReadClient struct {
	listPaths []string
	readPaths []string
}

func (c *fakeReadClient) List(_ context.Context, path string, _ int) (readserver.DirListing, error) {
	c.listPaths = append(c.listPaths, path)
	return readserver.DirListing{Entries: []readserver.Entry{{Name: "a.go", Type: "file", Size: 3}}, NextPage: 0}, nil
}

func (c *fakeReadClient) Read(_ context.Context, path string, offset, length int64) (readserver.FileContent, error) {
	c.readPaths = append(c.readPaths, path)
	return readserver.FileContent{Size: 3, ContentType: "text", Offset: offset, Length: 3, Data: []byte("abc")}, nil
}

func (c *fakeReadClient) Stat(_ context.Context, path string) (readserver.FileStat, error) {
	return readserver.FileStat{
		Name: path, Type: "file", Size: 3, ModTime: "2026-09-18T00:00:00Z",
		Git: &readserver.GitChange{
			CommitHash: "cafe", Author: "alice", Message: "touch", Timestamp: "2026-09-17T00:00:00Z",
		},
	}, nil
}

func newFakeReaper(t *testing.T, l readerpod.Launcher) *readerpod.Reaper {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(s).Build()
	return readerpod.NewReaper(c, l, "default", 0)
}

func validReaderSpec() readerpod.Spec {
	return readerpod.Spec{RunID: "run-9", ProjectPVCName: "pvc-9", CommitSHA: "cafe", ReaderSAName: "run-9-reader"}
}

// newTestReader wires the WorkspaceReader with the fakes and a no-op readiness wait.
func newTestReader(t *testing.T, resolver ReaderSpecResolver, l *fakeLauncher, rc readClient) *ReaderPodWorkspaceReader {
	r := NewReaderPodWorkspaceReader(resolver, l, newFakeReaper(t, l), time.Minute)
	r.dial = func(string) readClient { return rc }
	r.ready = func(context.Context, string) error { return nil }
	return r
}

// --- test helpers -------------------------------------------------------------------

// pollUntilReady polls ListDir until it returns something other than ErrReaderPreparing,
// or until timeout. It is the async-model analogue of the old synchronous session().
func pollUntilReady(t *testing.T, r *ReaderPodWorkspaceReader, projectID string) (*DirListing, error) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		dl, err := r.ListDir(context.Background(), projectID, ".", 0)
		if err == nil {
			return dl, nil
		}
		if !errors.Is(err, ErrReaderPreparing) {
			return nil, err
		}
		if time.Now().After(deadline) {
			t.Fatalf("pollUntilReady(%s): still ErrReaderPreparing after 2s", projectID)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// warmListSession polls ReadFile until the reader session is ready, so listing-cache tests can
// measure ListDir behavior without the async launch (ADR-0025 D1) racing the measurements.
// ReadFile does not touch the listing cache, so the measured ListDir sequence starts cold.
func warmListSession(t *testing.T, r *ReaderPodWorkspaceReader, projectID string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		_, err := r.ReadFile(context.Background(), projectID, "warmup", 0, 3)
		if err == nil {
			return
		}
		if !errors.Is(err, ErrReaderPreparing) {
			t.Fatalf("warmListSession(%s): %v", projectID, err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("warmListSession(%s): still ErrReaderPreparing after 2s", projectID)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitNoErr polls ListDir until it returns a non-preparing error or timeout.
func pollUntilError(t *testing.T, r *ReaderPodWorkspaceReader, projectID string) error {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		_, err := r.ListDir(context.Background(), projectID, ".", 0)
		if err != nil && !errors.Is(err, ErrReaderPreparing) {
			return err
		}
		if err == nil {
			t.Fatalf("pollUntilError(%s): got nil error, want terminal error", projectID)
		}
		if time.Now().After(deadline) {
			t.Fatalf("pollUntilError(%s): still ErrReaderPreparing after 2s", projectID)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// --- tests -------------------------------------------------------------------

// TestReaderPodWorkspaceReader_LaunchOncePerProject: the first read launches a reader; subsequent
// reads reuse the same session (one launch, not one per call).
func TestReaderPodWorkspaceReader_LaunchOncePerProject(t *testing.T) {
	l := &fakeLauncher{}
	rc := &fakeReadClient{}
	r := newTestReader(t, &fakeResolver{spec: validReaderSpec()}, l, rc)

	// First call returns ErrReaderPreparing; poll until warm.
	if _, err := pollUntilReady(t, r, "proj-1"); err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	if _, err := r.ReadFile(context.Background(), "proj-1", "a.go", 0, 0); err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	l.mu.Lock()
	launches := l.launches
	l.mu.Unlock()
	if launches != 1 {
		t.Errorf("launches = %d, want 1 (session reused)", launches)
	}
	if len(rc.listPaths) != 1 || rc.listPaths[0] != "." {
		t.Errorf("list paths = %v", rc.listPaths)
	}
	if len(rc.readPaths) != 1 || rc.readPaths[0] != "a.go" {
		t.Errorf("read paths = %v", rc.readPaths)
	}
}

// TestReaderPodWorkspaceReader_WireMapping: the pod wire types map onto the apiserver types.
func TestReaderPodWorkspaceReader_WireMapping(t *testing.T) {
	r := newTestReader(t, &fakeResolver{spec: validReaderSpec()}, &fakeLauncher{}, &fakeReadClient{})

	dl, err := pollUntilReady(t, r, "proj-1")
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	if len(dl.Entries) != 1 || dl.Entries[0].Name != "a.go" || dl.Entries[0].Type != "file" || dl.Entries[0].Size != 3 {
		t.Errorf("mapped listing = %+v", dl)
	}
	fc, err := r.ReadFile(context.Background(), "proj-1", "a.go", 0, 0)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(fc.Data) != "abc" || fc.ContentType != "text" || fc.Size != 3 {
		t.Errorf("mapped content = %+v", fc)
	}
}

// TestReaderPodWorkspaceReader_Containment: the client-supplied path never reaches the resolver — the
// Spec is derived from projectID alone (AC2/AC3), so a caller cannot widen the mount or SA.
func TestReaderPodWorkspaceReader_Containment(t *testing.T) {
	res := &fakeResolver{spec: validReaderSpec()}
	r := newTestReader(t, res, &fakeLauncher{}, &fakeReadClient{})

	// Warm the session so the resolver has been called.
	pollUntilReady(t, r, "proj-1")
	// A hostile path must not change what the resolver was asked (only the projectID).
	_, _ = r.ListDir(context.Background(), "proj-1", "../../etc/passwd", 0)
	if len(res.seen) != 1 || res.seen[0] != "proj-1" {
		t.Errorf("resolver saw %v, want exactly [proj-1] — path must not influence spec derivation", res.seen)
	}
}

// TestReaderPodWorkspaceReader_Busy: a resolver that reports the workspace busy surfaces as
// ErrWorkspaceBusy so the route degrades to the last-committed snapshot (AC7).
// With async launch the error arrives on the second poll (first call returns ErrReaderPreparing).
func TestReaderPodWorkspaceReader_Busy(t *testing.T) {
	r := newTestReader(t, &fakeResolver{err: ErrWorkspaceBusy}, &fakeLauncher{}, &fakeReadClient{})
	if err := pollUntilError(t, r, "proj-1"); !errors.Is(err, ErrWorkspaceBusy) {
		t.Fatalf("err = %v, want ErrWorkspaceBusy", err)
	}
}

// TestReaderPodWorkspaceReader_FlagOffDegrades: a DisabledLauncher (flag off) surfaces as
// ErrWorkspaceBusy, so a cluster that has not opted in degrades to snapshot-only.
func TestReaderPodWorkspaceReader_FlagOffDegrades(t *testing.T) {
	l := &fakeLauncher{launchErr: readerpod.ErrDisabled}
	r := NewReaderPodWorkspaceReader(&fakeResolver{spec: validReaderSpec()}, l, newFakeReaper(t, l), time.Minute)
	r.ready = func(context.Context, string) error { return nil }
	r.dial = func(string) readClient { return &fakeReadClient{} }
	if err := pollUntilError(t, r, "proj-1"); !errors.Is(err, ErrWorkspaceBusy) {
		t.Fatalf("flag-off err = %v, want ErrWorkspaceBusy", err)
	}
}

// TestReaderPodWorkspaceReader_NoBrowseTarget: a project with no completed Run surfaces
// ErrNoBrowseTarget unchanged for the route to render "nothing to browse".
func TestReaderPodWorkspaceReader_NoBrowseTarget(t *testing.T) {
	r := newTestReader(t, &fakeResolver{err: ErrNoBrowseTarget}, &fakeLauncher{}, &fakeReadClient{})
	if err := pollUntilError(t, r, "proj-1"); !errors.Is(err, ErrNoBrowseTarget) {
		t.Fatalf("err = %v, want ErrNoBrowseTarget", err)
	}
}

// TestReaderPodWorkspaceReader_Preparing202: the first cold request returns ErrReaderPreparing
// and a subsequent call succeeds once the background launch completes.
func TestReaderPodWorkspaceReader_Preparing202(t *testing.T) {
	r := newTestReader(t, &fakeResolver{spec: validReaderSpec()}, &fakeLauncher{}, &fakeReadClient{})
	// First call must return ErrReaderPreparing immediately (cold start).
	_, err := r.ListDir(context.Background(), "proj-cold", ".", 0)
	if !errors.Is(err, ErrReaderPreparing) {
		t.Fatalf("first cold call = %v, want ErrReaderPreparing", err)
	}
	// Subsequent polls must eventually return a real listing.
	if _, err := pollUntilReady(t, r, "proj-cold"); err != nil {
		t.Fatalf("pollUntilReady: %v", err)
	}
}

// TestReaderPodWorkspaceReader_SweepIdle_TearsDown: a session idle past the window is reaped and a
// later read re-launches (fresh session).
func TestReaderPodWorkspaceReader_SweepIdle_TearsDown(t *testing.T) {
	l := &fakeLauncher{}
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	r := newTestReader(t, &fakeResolver{spec: validReaderSpec()}, l, &fakeReadClient{})
	r.idle = 2 * time.Minute
	r.now = func() time.Time { return now }

	if _, err := pollUntilReady(t, r, "proj-1"); err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	// Advance the clock past the idle window and sweep.
	now = now.Add(5 * time.Minute)
	if reaped := r.SweepIdle(context.Background()); reaped != 1 {
		t.Fatalf("SweepIdle reaped %d, want 1", reaped)
	}
	l.mu.Lock()
	td := l.teardowns
	l.mu.Unlock()
	if td != 1 {
		t.Errorf("teardowns = %d, want 1", td)
	}
	// A subsequent read re-launches a fresh reader.
	if _, err := pollUntilReady(t, r, "proj-1"); err != nil {
		t.Fatalf("ListDir after sweep: %v", err)
	}
	l.mu.Lock()
	launches := l.launches
	l.mu.Unlock()
	if launches != 2 {
		t.Errorf("launches = %d, want 2 (relaunch after idle teardown)", launches)
	}
}

// TestReaderPodWorkspaceReader_ActiveSessionNotReaped: an actively-touched session survives a sweep.
func TestReaderPodWorkspaceReader_ActiveSessionNotReaped(t *testing.T) {
	l := &fakeLauncher{}
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	r := newTestReader(t, &fakeResolver{spec: validReaderSpec()}, l, &fakeReadClient{})
	r.idle = 10 * time.Minute
	r.now = func() time.Time { return now }

	if _, err := pollUntilReady(t, r, "proj-1"); err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	now = now.Add(1 * time.Minute)
	if reaped := r.SweepIdle(context.Background()); reaped != 0 {
		t.Fatalf("SweepIdle reaped %d, want 0 (still within idle window)", reaped)
	}
	l.mu.Lock()
	td := l.teardowns
	l.mu.Unlock()
	if td != 0 {
		t.Errorf("teardowns = %d, want 0", td)
	}
}

// TestListDirCache_DeduplicatesRapidReexpand: two ListDir calls within listCacheTTL should only
// reach the reader pod once (ADR-0025 D5 — short-TTL listing cache).
func TestListDirCache_DeduplicatesRapidReexpand(t *testing.T) {
	rc := &fakeReadClient{}
	r := newTestReader(t, &fakeResolver{spec: validReaderSpec()}, &fakeLauncher{}, rc)

	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }

	// Warm the session via ReadFile first: the async launch (D1) returns ErrReaderPreparing
	// from the first ListDir, which would otherwise be mistaken for a cache result.
	warmListSession(t, r, "proj-1")

	// First call — must hit the pod.
	if _, err := r.ListDir(context.Background(), "proj-1", ".", 0); err != nil {
		t.Fatalf("first ListDir: %v", err)
	}
	if len(rc.listPaths) != 1 {
		t.Fatalf("want 1 pod call after first ListDir, got %d", len(rc.listPaths))
	}

	// Second call within TTL — must be served from cache (pod call count unchanged).
	now = now.Add(listCacheTTL / 2)
	if _, err := r.ListDir(context.Background(), "proj-1", ".", 0); err != nil {
		t.Fatalf("second ListDir (within TTL): %v", err)
	}
	if len(rc.listPaths) != 1 {
		t.Errorf("want 1 pod call (cache hit), got %d", len(rc.listPaths))
	}

	// Third call after TTL — must re-hit the pod.
	now = now.Add(listCacheTTL)
	if _, err := r.ListDir(context.Background(), "proj-1", ".", 0); err != nil {
		t.Fatalf("third ListDir (after TTL): %v", err)
	}
	if len(rc.listPaths) != 2 {
		t.Errorf("want 2 pod calls after TTL expiry, got %d", len(rc.listPaths))
	}
}

// TestListDirCache_DifferentKeysNotShared: cache is keyed by (dirPath, page) so different paths
// and pages do not share entries.
func TestListDirCache_DifferentKeysNotShared(t *testing.T) {
	rc := &fakeReadClient{}
	r := newTestReader(t, &fakeResolver{spec: validReaderSpec()}, &fakeLauncher{}, rc)

	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }

	// Warm the session via ReadFile first so every ListDir below measures cache behavior,
	// not the async launch's ErrReaderPreparing window (ADR-0025 D1).
	warmListSession(t, r, "proj-1")

	if _, err := r.ListDir(context.Background(), "proj-1", ".", 0); err != nil {
		t.Fatalf("ListDir ./0: %v", err)
	}
	if _, err := r.ListDir(context.Background(), "proj-1", "src", 0); err != nil {
		t.Fatalf("ListDir src/0: %v", err)
	}
	if _, err := r.ListDir(context.Background(), "proj-1", ".", 1); err != nil {
		t.Fatalf("ListDir ./1: %v", err)
	}
	// All three are distinct keys — expect 3 pod calls.
	if len(rc.listPaths) != 3 {
		t.Errorf("want 3 pod calls for 3 distinct keys, got %d", len(rc.listPaths))
	}
}

// --- ISI-5431: author propagation through the background launch ----------------------------
//
// authRequiringResolver mimics the PRODUCTION CoordReaderSpecResolver (readerspec.go): resolution
// fails closed when the ctx carries no authenticated author. Before ISI-5431, session() discarded
// the request ctx and launchBackground resolved on a bare context.Background(), so every background
// launch failed with "readerspec: no author context" and the File Explorer looped 202↔500 forever.
//
// Note the middleware invariant modelled here: a BFF-stamped author ALWAYS has a Principal, so an
// author-less launch (ok=false) and a zero-value author (Principal="") both mean "unauthenticated"
// and fail closed.
type authRequiringResolver struct {
	inner *fakeResolver
}

func (a *authRequiringResolver) ResolveReaderSpec(ctx context.Context, projectID string) (readerpod.Spec, error) {
	author, ok := discussion.AuthFromContext(ctx)
	if !ok || author.Principal == "" {
		return readerpod.Spec{}, errors.New("readerspec: no author context (fail closed)")
	}
	return a.inner.ResolveReaderSpec(ctx, projectID)
}

// pollAuthedReady polls ListDir with ctx until the session warms (nil) or a terminal error fires.
func pollAuthedReady(t *testing.T, r *ReaderPodWorkspaceReader, ctx context.Context, projectID string) error {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		_, err := r.ListDir(ctx, projectID, ".", 0)
		if err == nil {
			return nil
		}
		if !errors.Is(err, ErrReaderPreparing) {
			return err
		}
		if time.Now().After(deadline) {
			t.Fatalf("pollAuthedReady(%s): still ErrReaderPreparing after 2s", projectID)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestReaderPodWorkspaceReader_BackgroundLaunchCarriesAuthor: a read issued on an AUTHENTICATED
// request ctx must warm successfully even though the resolver fails closed without an author —
// i.e. the author survives the hop from session() into the detached background launch ctx.
func TestReaderPodWorkspaceReader_BackgroundLaunchCarriesAuthor(t *testing.T) {
	res := &authRequiringResolver{inner: &fakeResolver{spec: validReaderSpec()}}
	r := newTestReader(t, res, &fakeLauncher{}, &fakeReadClient{})

	ctx := discussion.WithAuth(context.Background(), readerspecAuth())
	if err := pollAuthedReady(t, r, ctx, "proj-1"); err != nil {
		t.Fatalf("ListDir with authenticated ctx: %v (author was dropped from the background launch)", err)
	}
}

// TestReaderPodWorkspaceReader_PreWarmCarriesAuthor: PreWarm must capture the caller's author the
// same way — a pre-warm issued with an authenticated ctx must yield a usable session, not a
// fail-closed launch error.
func TestReaderPodWorkspaceReader_PreWarmCarriesAuthor(t *testing.T) {
	res := &authRequiringResolver{inner: &fakeResolver{spec: validReaderSpec()}}
	r := newTestReader(t, res, &fakeLauncher{}, &fakeReadClient{})

	ctx := discussion.WithAuth(context.Background(), readerspecAuth())
	r.PreWarm(ctx, "proj-1")
	if err := pollAuthedReady(t, r, ctx, "proj-1"); err != nil {
		t.Fatalf("ListDir after authenticated PreWarm: %v (author was dropped from the background launch)", err)
	}
}

// TestReaderPodWorkspaceReader_UnauthenticatedLaunchFailsClosed: negative control — with NO author
// in the request ctx the fail-closed resolver error must surface (this is the pre-ISI-5431
// behaviour every caller saw, and the reason the author capture above is required).
func TestReaderPodWorkspaceReader_UnauthenticatedLaunchFailsClosed(t *testing.T) {
	res := &authRequiringResolver{inner: &fakeResolver{spec: validReaderSpec()}}
	r := newTestReader(t, res, &fakeLauncher{}, &fakeReadClient{})

	err := pollAuthedReady(t, r, context.Background(), "proj-1")
	if err == nil || !strings.Contains(err.Error(), "no author context") {
		t.Fatalf("err = %v, want fail-closed no-author-context error", err)
	}
}
