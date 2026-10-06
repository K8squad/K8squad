package apiserver

// D5b tier-1 listing-cache tests (ISI-5499, ADR-0025 §D5b). Smallest checks that prove the
// acceptance criteria:
//   - N concurrent viewers at the same generation trigger exactly ONE walk (single-flight).
//   - A subsequent viewer is served from the in-process cache (no second walk).
//   - A generation change is never served stale (old entry unreachable by key).
//   - The LRU honours its TTL and its entry bound (bounded memory).

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// countingGenReader is a WorkspaceReader + GenerationResolver that counts ListDir calls and can
// block inside ListDir (to force a concurrent overlap) so single-flight collapse is observable.
type countingGenReader struct {
	calls atomic.Int64

	mu      sync.Mutex
	gen     string
	block   chan struct{} // when non-nil, ListDir waits on it before returning
	entered chan struct{} // ListDir signals here once it is executing (buffered)
}

func (f *countingGenReader) ListDir(_ context.Context, _, dirPath string, page int) (*DirListing, error) {
	f.calls.Add(1)
	if f.entered != nil {
		select {
		case f.entered <- struct{}{}:
		default:
		}
	}
	if f.block != nil {
		<-f.block
	}
	f.mu.Lock()
	gen := f.gen
	f.mu.Unlock()
	// Fresh listing each call (the real reader builds one per walk); stamp the current generation.
	return &DirListing{
		Entries:    []DirEntry{{Name: "main.go", Type: "file", Size: 10}},
		Generation: gen,
	}, nil
}

func (f *countingGenReader) ReadFile(_ context.Context, _, _ string, _, _ int64) (*FileContent, error) {
	return &FileContent{}, nil
}
func (f *countingGenReader) StatFile(_ context.Context, _, _ string) (*FileStat, error) {
	return &FileStat{}, nil
}
func (f *countingGenReader) ResolveGeneration(_ context.Context, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gen, nil
}

func newCacheTestServer() *Server {
	return &Server{listingCache: newListingCache()}
}

// TestCachedListDir_ConcurrentFanOut_OneWalk — N concurrent viewers of the same project+generation
// collapse to exactly ONE reader.ListDir (the structural O(users) PVC-walk redundancy, removed).
func TestCachedListDir_ConcurrentFanOut_OneWalk(t *testing.T) {
	reader := &countingGenReader{gen: "genA", block: make(chan struct{}), entered: make(chan struct{}, 1)}
	s := newCacheTestServer()

	const n = 12
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			listing, err := s.cachedListDir(context.Background(), reader, "proj-1", ".", 0)
			if err != nil {
				t.Errorf("cachedListDir: %v", err)
				return
			}
			if len(listing.Entries) != 1 || listing.Entries[0].Name != "main.go" {
				t.Errorf("unexpected listing: %+v", listing.Entries)
			}
		}()
	}

	// Wait until the single-flight winner is executing ListDir, give the rest time to queue on Do,
	// then release the walk. All N share the one in-flight call.
	<-reader.entered
	time.Sleep(50 * time.Millisecond)
	close(reader.block)
	wg.Wait()

	if got := reader.calls.Load(); got != 1 {
		t.Fatalf("concurrent fan-out: ListDir called %d times, want exactly 1", got)
	}
}

// TestCachedListDir_SubsequentServedFromCache — after the first walk, a later viewer is served from
// the in-process cache without a second walk.
func TestCachedListDir_SubsequentServedFromCache(t *testing.T) {
	reader := &countingGenReader{gen: "genA"}
	s := newCacheTestServer()

	for i := 0; i < 5; i++ {
		if _, err := s.cachedListDir(context.Background(), reader, "proj-1", ".", 0); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if got := reader.calls.Load(); got != 1 {
		t.Fatalf("subsequent viewers: ListDir called %d times, want 1", got)
	}
	hits, _, _ := s.listingCache.stats()
	if hits < 4 {
		t.Fatalf("expected >=4 cache hits, got %d", hits)
	}
}

// TestCachedListDir_GenerationChangeNeverStale — a new generation uses a new key, so the old entry
// is unreachable and a fresh walk runs; the served listing reflects the new generation.
func TestCachedListDir_GenerationChangeNeverStale(t *testing.T) {
	reader := &countingGenReader{gen: "genA"}
	s := newCacheTestServer()

	first, err := s.cachedListDir(context.Background(), reader, "proj-1", ".", 0)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if first.Generation != "genA" {
		t.Fatalf("first generation = %q, want genA", first.Generation)
	}

	// Flip generation (e.g. a new succeeded browse-target). The old key must not be hit.
	reader.mu.Lock()
	reader.gen = "genB"
	reader.mu.Unlock()

	second, err := s.cachedListDir(context.Background(), reader, "proj-1", ".", 0)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if second.Generation != "genB" {
		t.Fatalf("after flip generation = %q, want genB (stale genA served!)", second.Generation)
	}
	if got := reader.calls.Load(); got != 2 {
		t.Fatalf("generation change: ListDir called %d times, want 2 (one per generation)", got)
	}
}

// TestCachedListDir_NoGenerationDisablesCache — a reader without GenerationResolver support (empty
// token) is never cached: every call walks, exactly the pre-D5b behaviour.
func TestCachedListDir_NoGenerationDisablesCache(t *testing.T) {
	reader := &fakeWorkspaceReader{listing: &DirListing{Entries: []DirEntry{{Name: "x", Type: "file"}}}}
	s := newCacheTestServer()
	for i := 0; i < 3; i++ {
		if _, err := s.cachedListDir(context.Background(), reader, "proj-1", ".", 0); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if _, misses, _ := s.listingCache.stats(); misses != 0 {
		t.Fatalf("no-generation reader should bypass the cache entirely, got %d misses", misses)
	}
}

// TestCachedListDir_DegradedNeverCached — a degraded listing (busy snapshot) must not be stored,
// so a later clean listing at the same key is not shadowed by stale degraded bytes.
func TestCachedListDir_DegradedNeverCached(t *testing.T) {
	s := newCacheTestServer()
	// A reader that returns a DEGRADED listing carrying a generation. cachedListDir must refuse it.
	degraded := &stubGenListing{gen: "genA", listing: &DirListing{
		Entries: []DirEntry{}, Degraded: true, Reason: reasonWorkspaceBusy, Generation: "genA",
	}}
	if _, err := s.cachedListDir(context.Background(), degraded, "proj-1", ".", 0); err != nil {
		t.Fatalf("degraded call: %v", err)
	}
	if _, ok := s.listingCache.get(listingKey("proj-1", "genA", ".", 0)); ok {
		t.Fatal("degraded listing was cached — must never be stored")
	}
}

// stubGenListing returns a fixed (possibly degraded) listing and a generation.
type stubGenListing struct {
	gen     string
	listing *DirListing
}

func (f *stubGenListing) ListDir(_ context.Context, _, _ string, _ int) (*DirListing, error) {
	return f.listing, nil
}
func (f *stubGenListing) ReadFile(_ context.Context, _, _ string, _, _ int64) (*FileContent, error) {
	return &FileContent{}, nil
}
func (f *stubGenListing) StatFile(_ context.Context, _, _ string) (*FileStat, error) {
	return &FileStat{}, nil
}
func (f *stubGenListing) ResolveGeneration(_ context.Context, _ string) (string, error) {
	return f.gen, nil
}

// ---- listingCache unit checks (TTL + bounds) with an injected clock ------------------------------

func TestListingCache_TTLExpiry(t *testing.T) {
	now := time.Unix(1000, 0)
	c := newListingCache()
	c.now = func() time.Time { return now }

	c.put("k", &DirListing{Entries: []DirEntry{{Name: "a"}}})
	if _, ok := c.get("k"); !ok {
		t.Fatal("expected hit immediately after put")
	}
	now = now.Add(listingCacheTTL + time.Second)
	if _, ok := c.get("k"); ok {
		t.Fatal("expected miss after TTL expiry")
	}
}

func TestListingCache_EntryBoundEvictsLRU(t *testing.T) {
	now := time.Unix(2000, 0)
	c := newListingCache()
	c.now = func() time.Time { return now }
	c.maxEntries = 3

	for _, k := range []string{"a", "b", "c"} {
		c.put(k, &DirListing{Entries: []DirEntry{{Name: k}}})
	}
	// Touch "a" so it is most-recently-used; "b" becomes the eviction victim.
	if _, ok := c.get("a"); !ok {
		t.Fatal("a should still be present")
	}
	c.put("d", &DirListing{Entries: []DirEntry{{Name: "d"}}})

	if _, ok := c.get("b"); ok {
		t.Fatal("b should have been evicted as LRU")
	}
	for _, k := range []string{"a", "c", "d"} {
		if _, ok := c.get(k); !ok {
			t.Fatalf("%s should still be present", k)
		}
	}
	if _, _, ev := c.stats(); ev != 1 {
		t.Fatalf("expected exactly 1 eviction, got %d", ev)
	}
}
