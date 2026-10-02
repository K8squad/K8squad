package apiserver

// ReaderPodWorkspaceReader is the S4b-wire (ISI-4072) implementation of WorkspaceReader (files.go):
// it satisfies the project file-explorer read seam by launching the story 8.7f / S4a reader pod on
// demand and calling its in-cluster ClusterIP /list + /read protocol (readclient), mapping the pod
// wire types (readserver.DirListing/FileContent) → the apiserver DirListing/FileContent.
//
// It wires together, and nothing else:
//   - a ReaderSpecResolver (AC2/AC3 containment) — the reader-pod Spec (PVC / commit / SA) is derived
//     SERVER-SIDE from the coord record, keyed only by projectID. The client-supplied path never
//     influences the Spec, so a caller can never widen the mount or credential scope.
//   - a readerpod.Launcher (AC2) — on-demand Launch of the pod + paired ClusterIP Service; TearDown
//     on session end / idle.
//   - a readerpod.Reaper (AC6) — the single metering teardown path for idle sessions.
//
// There is NO pods/exec and NO kubectl cp anywhere on this path; the only cluster reach is the
// launcher's pod+service create/delete and an in-cluster HTTP GET to the reader.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/K8squad/K8squad/internal/buildbrowser/readerpod"
	"github.com/K8squad/K8squad/internal/buildbrowser/readerpod/readclient"
	"github.com/K8squad/K8squad/internal/buildbrowser/readerpod/readserver"
)

// ErrNoBrowseTarget is returned by a ReaderSpecResolver when a project has no completed Run / PVC to
// browse yet. The route maps it to an empty (non-degraded) listing so S4c renders "nothing to browse"
// rather than an error.
var ErrNoBrowseTarget = errors.New("apiserver: project has no browsable workspace yet")

// ReaderSpecResolver derives the SERVER-SIDE reader-pod Spec for a project's browse target. Every
// field (PVC, commit, reader SA) comes from the coord record — never a request body — which is the
// AC2/AC3 containment boundary: the file-explorer routes pass only projectID, so the client can never
// widen the mount or credential scope. The returned Spec MUST already satisfy readerpod.Spec.Validate.
//
// A resolver signals two first-class conditions the route degrades rather than 5xx's:
//   - ErrWorkspaceBusy: the target PVC is RWO and currently held by a running agent, so a read-only
//     co-mount is impossible (AC7) — the route falls back to the last-committed snapshot (GitReader).
//   - ErrNoBrowseTarget: the project has no completed Run/PVC yet.
type ReaderSpecResolver interface {
	ResolveReaderSpec(ctx context.Context, projectID string) (readerpod.Spec, error)
}

// readClient is the narrow reader-pod protocol surface ReaderPodWorkspaceReader consumes.
// *readclient.Client implements it; tests supply a fake bound to an httptest server.
type readClient interface {
	List(ctx context.Context, path string, page int) (readserver.DirListing, error)
	Read(ctx context.Context, path string, offset, length int64) (readserver.FileContent, error)
	Stat(ctx context.Context, path string) (readserver.FileStat, error)
}

// readerSession is one project's live reader pod: its teardown Handle, the bound protocol client, and
// the last time a read touched it (for idle-teardown).
type readerSession struct {
	handle     readerpod.Handle
	client     readClient
	lastAccess time.Time
}

// ReaderPodWorkspaceReader implements WorkspaceReader by managing per-project reader-pod sessions.
type ReaderPodWorkspaceReader struct {
	resolver ReaderSpecResolver
	launcher readerpod.Launcher
	reaper   *readerpod.Reaper

	// dial builds a protocol client for a reader's in-cluster base URL. Overridable in tests.
	dial func(baseURL string) readClient
	// ready blocks until the reader at baseURL answers /healthz, or ctx/timeout elapses. Overridable
	// in tests. A nil ready skips the readiness wait (used only when dial is faked).
	ready func(ctx context.Context, baseURL string) error

	idle time.Duration
	now  func() time.Time

	mu         sync.Mutex
	sessions   map[string]*readerSession
	// preparing tracks projects whose reader pod is launching in the background (ADR-0025 D1/S2a).
	// Entries are added at launch start and removed when the launch completes (success or failure).
	preparing  map[string]struct{}
	// lastErr stores the terminal error from the most recent failed background launch for a project.
	// session() surfaces it on the next call and then clears it, so callers polling after a 202
	// eventually see the real error (ErrWorkspaceBusy, ErrNoBrowseTarget, …) rather than looping.
	lastErr    map[string]error
}

// NewReaderPodWorkspaceReader builds the S4b-wire WorkspaceReader. idle is the no-read window after
// which a session's reader pod is torn down; a zero idle defaults to defaultReaderIdle.
func NewReaderPodWorkspaceReader(resolver ReaderSpecResolver, launcher readerpod.Launcher, reaper *readerpod.Reaper, idle time.Duration) *ReaderPodWorkspaceReader {
	if idle <= 0 {
		idle = defaultReaderIdle
	}
	return &ReaderPodWorkspaceReader{
		resolver:  resolver,
		launcher:  launcher,
		reaper:    reaper,
		dial:      func(baseURL string) readClient { return readclient.New(baseURL, nil) },
		ready:     waitReaderReady,
		idle:      idle,
		now:       time.Now,
		sessions:  map[string]*readerSession{},
		preparing: map[string]struct{}{},
		lastErr:   map[string]error{},
	}
}

const defaultReaderIdle = 5 * time.Minute

// ListDir launches-or-reuses the project's reader pod and returns its directory listing, mapped to
// the apiserver wire type. A busy workspace surfaces as ErrWorkspaceBusy (the route degrades).
func (r *ReaderPodWorkspaceReader) ListDir(ctx context.Context, projectID, dirPath string, page int) (*DirListing, error) {
	sess, err := r.session(ctx, projectID)
	if err != nil {
		return nil, err
	}
	wire, err := sess.client.List(ctx, dirPath, page)
	if err != nil {
		return nil, mapReadErr(err)
	}
	entries := make([]DirEntry, 0, len(wire.Entries))
	for _, e := range wire.Entries {
		entries = append(entries, DirEntry{Name: e.Name, Type: e.Type, Size: e.Size})
	}
	return &DirListing{Entries: entries, NextPage: wire.NextPage}, nil
}

// ReadFile launches-or-reuses the project's reader pod and returns file content, mapped to the
// apiserver wire type. A busy workspace surfaces as ErrWorkspaceBusy.
func (r *ReaderPodWorkspaceReader) ReadFile(ctx context.Context, projectID, filePath string, offset, length int64) (*FileContent, error) {
	sess, err := r.session(ctx, projectID)
	if err != nil {
		return nil, err
	}
	wire, err := sess.client.Read(ctx, filePath, offset, length)
	if err != nil {
		return nil, mapReadErr(err)
	}
	return &FileContent{
		Data:        wire.Data,
		Size:        wire.Size,
		ContentType: wire.ContentType,
		Offset:      wire.Offset,
		Length:      wire.Length,
	}, nil
}

// StatFile launches-or-reuses the project's reader pod and returns change metadata for filePath
// (ISI-4649), mapped to the apiserver wire type. A nil Git in the pod response is the graceful
// no-git fallback and flows through as-is. A busy workspace surfaces as ErrWorkspaceBusy.
func (r *ReaderPodWorkspaceReader) StatFile(ctx context.Context, projectID, filePath string) (*FileStat, error) {
	sess, err := r.session(ctx, projectID)
	if err != nil {
		return nil, err
	}
	wire, err := sess.client.Stat(ctx, filePath)
	if err != nil {
		return nil, mapReadErr(err)
	}
	st := &FileStat{
		Name:    wire.Name,
		Type:    wire.Type,
		Size:    wire.Size,
		ModTime: wire.ModTime,
	}
	if wire.Git != nil {
		st.Git = &GitChange{
			CommitHash: wire.Git.CommitHash,
			Author:     wire.Git.Author,
			Message:    wire.Git.Message,
			Timestamp:  wire.Git.Timestamp,
		}
	}
	return st, nil
}

// ErrReaderPreparing is returned by session() when the reader pod for a project is launching in the
// background but is not yet ready. Routes map this to HTTP 202 (ADR-0025 D1/S2c) so the client
// polls with back-off instead of hanging until the 45 s per-request timeout fires.
var ErrReaderPreparing = errors.New("apiserver: reader pod is warming up; retry shortly")

// PreWarm starts a background launch for projectID if no warm or in-flight session exists.
// It is a fire-and-forget call: the first GET on the project's files routes triggers it automatically,
// but callers can also invoke it proactively on project open (S2a) to reduce the first-request latency.
func (r *ReaderPodWorkspaceReader) PreWarm(projectID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.sessions[projectID]; ok {
		return // already warm
	}
	if _, ok := r.preparing[projectID]; ok {
		return // already launching
	}
	r.preparing[projectID] = struct{}{}
	go r.launchBackground(projectID) //nolint:gosec // G118: intentional — launch outlives the caller; uses its own 90s timeout inside
}

// session returns the live reader session for projectID (ADR-0025 D1/S2c). Unlike the original
// synchronous launch, this method never blocks waiting for pod readiness: if no warm session exists
// it starts a background launch and immediately returns ErrReaderPreparing, which the route translates
// to HTTP 202 so the client polls rather than timing out. Subsequent calls return the ready session
// once launchBackground completes, or the terminal error if the launch failed.
//
// It touches lastAccess on every successful call so an actively-browsed project is never reaped.
func (r *ReaderPodWorkspaceReader) session(_ context.Context, projectID string) (*readerSession, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if sess, ok := r.sessions[projectID]; ok {
		sess.lastAccess = r.now()
		return sess, nil
	}

	// Surface a stored terminal error from the most recent failed launch (e.g. ErrWorkspaceBusy,
	// ErrNoBrowseTarget). Clear it so the next poll kicks off a fresh attempt.
	if err, ok := r.lastErr[projectID]; ok {
		delete(r.lastErr, projectID)
		return nil, err
	}

	// Reader not warm — kick off a background launch if not already in flight.
	if _, ok := r.preparing[projectID]; !ok {
		r.preparing[projectID] = struct{}{}
		go r.launchBackground(projectID) //nolint:gosec // G118: intentional — launch outlives the request; uses its own 90s timeout inside
	}
	return nil, ErrReaderPreparing
}

// launchBackground launches the reader pod for projectID in a background goroutine (ADR-0025 D1/S2a).
// On success it stores the ready session; on failure it removes the preparing entry so the next
// session() call retries. The launch uses its own 90 s timeout independent of any client request.
func (r *ReaderPodWorkspaceReader) launchBackground(projectID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// AC2/AC3: spec is derived server-side from the coord record.
	spec, err := r.resolver.ResolveReaderSpec(ctx, projectID)
	if err == nil {
		err = spec.Validate()
		if err != nil {
			err = fmt.Errorf("readerpodreader: resolver returned an invalid spec: %w", err)
		}
	}

	var handle readerpod.Handle
	if err == nil {
		handle, err = r.launcher.Launch(ctx, spec)
		if errors.Is(err, readerpod.ErrDisabled) {
			err = ErrWorkspaceBusy
		} else if err != nil {
			err = fmt.Errorf("readerpodreader: launch reader for project %s: %w", projectID, err)
		}
	}

	var client readClient
	if err == nil && r.ready != nil {
		if readyErr := r.ready(ctx, handle.BaseURL()); readyErr != nil {
			_ = r.reaper.ReapHandle(context.Background(), handle)
			err = ErrWorkspaceBusy
		} else {
			client = r.dial(handle.BaseURL())
		}
	} else if err == nil {
		client = r.dial(handle.BaseURL())
	}

	r.mu.Lock()
	delete(r.preparing, projectID)
	if err == nil {
		r.sessions[projectID] = &readerSession{handle: handle, client: client, lastAccess: r.now()}
	} else {
		// Store terminal error so the next session() call surfaces it instead of looping on
		// ErrReaderPreparing. The error is consumed once (cleared on read) so the caller after
		// that gets a fresh ErrReaderPreparing and a new launch attempt.
		r.lastErr[projectID] = err
	}
	r.mu.Unlock()
}

// SweepIdle tears down every session idle for longer than r.idle, plus a cluster-side orphan sweep
// for readers this process lost track of (e.g. after a restart). It returns the number of sessions
// reaped. In-flight launches (preparing map) are never reaped here — they have their own 90 s timeout.
// A host drives it on a ticker (see Run); it is exported so a test can drive one sweep.
func (r *ReaderPodWorkspaceReader) SweepIdle(ctx context.Context) int {
	cutoff := r.now().Add(-r.idle)

	r.mu.Lock()
	var idle []struct {
		projectID string
		handle    readerpod.Handle
	}
	for pid, sess := range r.sessions {
		if sess.lastAccess.Before(cutoff) {
			idle = append(idle, struct {
				projectID string
				handle    readerpod.Handle
			}{pid, sess.handle})
			delete(r.sessions, pid)
		}
	}
	r.mu.Unlock()

	for _, e := range idle {
		_ = r.reaper.ReapHandle(ctx, e.handle)
	}
	// Backstop: reclaim any reader pods with no in-memory session (orphans).
	_, _ = r.reaper.ReapIdle(ctx)
	return len(idle)
}

// Run drives SweepIdle on interval until ctx is cancelled. The host starts it in a goroutine; the
// pod's 900s ActiveDeadline remains the kubelet-level backstop if this loop ever stops (AC6).
func (r *ReaderPodWorkspaceReader) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = r.idle
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.SweepIdle(ctx)
		}
	}
}

// mapReadErr translates a reader-pod protocol error into the seam's error contract. It is the single
// classification point so pod status codes and context errors never leak verbatim into the route layer.
//
//   - 404 from the pod → path-not-found (route 500s today; a future story can surface 404).
//   - 400 "not a directory" → ErrNotDirectory (download route fall-back, ISI-4650).
//   - context.DeadlineExceeded / context.Canceled on the per-request timeout → ErrReaderTimeout
//     (route returns retryable-degraded 503, ADR-0025 D2).
func mapReadErr(err error) error {
	// ADR-0025 D2: a timed-out reader call becomes a typed retryable-degraded error so the
	// route can return a structured 503 instead of a bare 500.
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w: %v", ErrReaderTimeout, err)
	}
	var se *readclient.StatusError
	if errors.As(err, &se) {
		switch se.Code {
		case http.StatusNotFound:
			return fmt.Errorf("workspace path not found: %w", err)
		case http.StatusBadRequest:
			// The readserver answers 400 "not a directory" when /list targets a regular
			// file — the download route (ISI-4650) relies on this to fall back to the
			// file path. Other 400s stay a generic invalid-path.
			if strings.Contains(se.Body, "not a directory") {
				return ErrNotDirectory
			}
			return fmt.Errorf("invalid workspace path: %w", err)
		}
	}
	return err
}

// waitReaderReady polls the reader pod's /healthz until it answers 200 or the context/timeout
// elapses. It is the default readiness wait; tests override it. It uses a short per-attempt timeout
// so a slow-starting pod is retried rather than blocking a full request timeout on the first attempt.
func waitReaderReady(ctx context.Context, baseURL string) error {
	if baseURL == "" {
		return errors.New("readerpodreader: empty reader base URL")
	}
	deadline := time.Now().Add(30 * time.Second)
	hc := &http.Client{Timeout: 2 * time.Second}
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/healthz", nil)
		resp, err := hc.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("readerpodreader: reader %s not ready within deadline", baseURL)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// compile-time proof the type satisfies the seam.
var _ WorkspaceReader = (*ReaderPodWorkspaceReader)(nil)
