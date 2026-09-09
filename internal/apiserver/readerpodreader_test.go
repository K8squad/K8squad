package apiserver

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/K8squad/K8squad/internal/buildbrowser/readerpod"
	"github.com/K8squad/K8squad/internal/buildbrowser/readerpod/readserver"
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

// --- tests -------------------------------------------------------------------

// TestReaderPodWorkspaceReader_LaunchOncePerProject: the first read launches a reader; subsequent
// reads reuse the same session (one launch, not one per call).
func TestReaderPodWorkspaceReader_LaunchOncePerProject(t *testing.T) {
	l := &fakeLauncher{}
	rc := &fakeReadClient{}
	r := newTestReader(t, &fakeResolver{spec: validReaderSpec()}, l, rc)

	if _, err := r.ListDir(context.Background(), "proj-1", "dir", 0); err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	if _, err := r.ReadFile(context.Background(), "proj-1", "a.go", 0, 0); err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if l.launches != 1 {
		t.Errorf("launches = %d, want 1 (session reused)", l.launches)
	}
	if len(rc.listPaths) != 1 || rc.listPaths[0] != "dir" {
		t.Errorf("list paths = %v", rc.listPaths)
	}
	if len(rc.readPaths) != 1 || rc.readPaths[0] != "a.go" {
		t.Errorf("read paths = %v", rc.readPaths)
	}
}

// TestReaderPodWorkspaceReader_WireMapping: the pod wire types map onto the apiserver types.
func TestReaderPodWorkspaceReader_WireMapping(t *testing.T) {
	r := newTestReader(t, &fakeResolver{spec: validReaderSpec()}, &fakeLauncher{}, &fakeReadClient{})

	dl, err := r.ListDir(context.Background(), "proj-1", ".", 0)
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

	// A hostile path must not change what the resolver is asked (only the projectID).
	_, _ = r.ListDir(context.Background(), "proj-1", "../../etc/passwd", 0)
	if len(res.seen) != 1 || res.seen[0] != "proj-1" {
		t.Errorf("resolver saw %v, want exactly [proj-1] — path must not influence spec derivation", res.seen)
	}
}

// TestReaderPodWorkspaceReader_Busy: a resolver that reports the workspace busy surfaces as
// ErrWorkspaceBusy so the route degrades to the last-committed snapshot (AC7).
func TestReaderPodWorkspaceReader_Busy(t *testing.T) {
	r := newTestReader(t, &fakeResolver{err: ErrWorkspaceBusy}, &fakeLauncher{}, &fakeReadClient{})
	if _, err := r.ListDir(context.Background(), "proj-1", ".", 0); !errors.Is(err, ErrWorkspaceBusy) {
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
	if _, err := r.ListDir(context.Background(), "proj-1", ".", 0); !errors.Is(err, ErrWorkspaceBusy) {
		t.Fatalf("flag-off err = %v, want ErrWorkspaceBusy", err)
	}
}

// TestReaderPodWorkspaceReader_NoBrowseTarget: a project with no completed Run surfaces
// ErrNoBrowseTarget unchanged for the route to render "nothing to browse".
func TestReaderPodWorkspaceReader_NoBrowseTarget(t *testing.T) {
	r := newTestReader(t, &fakeResolver{err: ErrNoBrowseTarget}, &fakeLauncher{}, &fakeReadClient{})
	if _, err := r.ReadFile(context.Background(), "proj-1", "a.go", 0, 0); !errors.Is(err, ErrNoBrowseTarget) {
		t.Fatalf("err = %v, want ErrNoBrowseTarget", err)
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

	if _, err := r.ListDir(context.Background(), "proj-1", ".", 0); err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	// Advance the clock past the idle window and sweep.
	now = now.Add(5 * time.Minute)
	if reaped := r.SweepIdle(context.Background()); reaped != 1 {
		t.Fatalf("SweepIdle reaped %d, want 1", reaped)
	}
	if l.teardowns != 1 {
		t.Errorf("teardowns = %d, want 1", l.teardowns)
	}
	// A subsequent read re-launches a fresh reader.
	if _, err := r.ListDir(context.Background(), "proj-1", ".", 0); err != nil {
		t.Fatalf("ListDir after sweep: %v", err)
	}
	if l.launches != 2 {
		t.Errorf("launches = %d, want 2 (relaunch after idle teardown)", l.launches)
	}
}

// TestReaderPodWorkspaceReader_ActiveSessionNotReaped: an actively-touched session survives a sweep.
func TestReaderPodWorkspaceReader_ActiveSessionNotReaped(t *testing.T) {
	l := &fakeLauncher{}
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	r := newTestReader(t, &fakeResolver{spec: validReaderSpec()}, l, &fakeReadClient{})
	r.idle = 10 * time.Minute
	r.now = func() time.Time { return now }

	if _, err := r.ListDir(context.Background(), "proj-1", ".", 0); err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	now = now.Add(1 * time.Minute)
	if reaped := r.SweepIdle(context.Background()); reaped != 0 {
		t.Fatalf("SweepIdle reaped %d, want 0 (still within idle window)", reaped)
	}
	if l.teardowns != 0 {
		t.Errorf("teardowns = %d, want 0", l.teardowns)
	}
}
