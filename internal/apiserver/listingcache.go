package apiserver

// D5b tier-1 — server-side in-process directory-LISTING cache (ISI-5499, ADR-0025 §D5b).
//
// Why this exists (the multi-user driver): D5a's cache (ISI-5485) is per-browser — it only
// saves a user from re-walking a tree *they* already loaded. For N distinct users viewing the
// SAME project, each still pays the full PVC walk. This cache is keyed on
// (projectID, generation-token) + (dirPath, page) so N concurrent viewers of the same project
// at the same generation share ONE walk instead of O(users) walks. It is the structural
// O(users) redundancy removed, with zero new infra (single apiserver replica / sticky routing
// is today's deployment — see ADR-0025 §D5b "tier-1, in-process").
//
// Two mechanisms, both load-bearing:
//
//   - single-flight (group): a BURST of N concurrent misses for the same key collapses to ONE
//     reader.ListDir call. This is always correct — it only dedups genuinely in-flight calls and
//     retains nothing — so it carries the "exactly ONE walk per generation" acceptance guarantee
//     for the concurrent-fan-out case even if the LRU is empty or disabled.
//   - bounded TTL LRU (items/ll): serves users who arrive AFTER the first walk returns (the
//     "subsequent users served from cache" acceptance criterion), within listingCacheTTL.
//
// What it caches: listing / tree-metadata ONLY (names/types/sizes + generation). NEVER file
// content — content continues to stream from the reader pod (ADR-0025 §D5b: the Redis
// content-blob idea was rejected as a mirror of a by-the-second-mutable PVC). Busy / degraded /
// no-browse-target responses are NEVER cached (they carry a busy-epoch or empty generation and
// must stay live).
//
// Invalidation is via the KEY: a new generation (busy↔idle flip or a new succeeded browse-target)
// simply never hits an old entry, so old entries are unreachable and age out by TTL.
//
// ponytail ceiling (TTL is deliberately SHORT, not "long"): ADR-0025 §D5a coherence note C2 — a
// FAILED/CANCELLED run flips busy→idle WITHOUT advancing the succeeded browse-target, so generation
// returns to the IDENTICAL idle token even though the live tree may have changed. generation alone
// does NOT guard that edge (the D5a client guards it with mandatory revalidate-on-hit). A server
// cache keyed on generation would, with a LONG TTL, serve the stale pre-run listing for the whole
// TTL and so defeat the client's self-healing. listingCacheTTL is therefore kept well BELOW a
// typical run duration: the stale pre-run entry expires DURING the busy window (we never refresh it
// while busy) and is gone before the idle flip, so the first post-run request misses and re-walks.
// Upgrade path (D5b tier-2, gated — see issue): if cross-replica recompute forces a Redis tier,
// pair it with run-lifecycle invalidation rather than lengthening this TTL.

import (
	"container/list"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"
)

// listingCacheTTL bounds how long a cached listing is served (see the failed-run ceiling above).
// Short on purpose: long enough to collapse a realistic open-the-project fan-out burst + the
// client revalidation storm that follows, short enough to self-heal the failed-run coherence edge.
const listingCacheTTL = 30 * time.Second

// listingCacheMaxEntries caps the number of cached (project,generation,path,page) listings.
// Listings are metadata-only so each is small; this is the LRU eviction bound.
const listingCacheMaxEntries = 1024

// listingCacheMaxBytes caps the approximate total bytes held across all cached listings — the
// explicit memory bound the ADR requires alongside the entry cap. 32 MiB is generous for
// metadata-only listings on a single replica and hard-bounds a pathological wide-tree burst.
const listingCacheMaxBytes = 32 << 20

// listingCacheEntry is one cached directory listing and its expiry.
type listingCacheEntry struct {
	key     string
	listing *DirListing
	bytes   int
	expires time.Time
}

// listingCache is a bounded, TTL'd, single-flighted in-process LRU of directory listings.
// All methods are safe for concurrent use.
type listingCache struct {
	mu       sync.Mutex
	ll       *list.List               // front = most-recently-used; back = eviction victim
	items    map[string]*list.Element // key → element holding *listingCacheEntry
	curBytes int

	maxEntries int
	maxBytes   int
	ttl        time.Duration
	now        func() time.Time

	// group collapses concurrent misses for the same key into one reader.ListDir call.
	group singleflight.Group

	// Best-effort counters for the D5b telemetry story (ISI-5486); read via stats().
	hits      atomic.Uint64
	misses    atomic.Uint64
	evictions atomic.Uint64
}

// newListingCache builds the cache with the package default bounds and the real clock.
func newListingCache() *listingCache {
	return &listingCache{
		ll:         list.New(),
		items:      make(map[string]*list.Element),
		maxEntries: listingCacheMaxEntries,
		maxBytes:   listingCacheMaxBytes,
		ttl:        listingCacheTTL,
		now:        time.Now,
	}
}

// get returns a cached listing for key when present and unexpired, marking it most-recently-used.
// An expired hit is removed and reported as a miss. The returned *DirListing is SHARED (not cloned):
// the OK-path listing is read-only to every caller (writeJSON / stream iteration only mutate on the
// degraded branches, which are never cached), so sharing avoids a per-hit copy of wide listings.
func (c *listingCache) get(key string) (*DirListing, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		c.misses.Add(1)
		return nil, false
	}
	ent := el.Value.(*listingCacheEntry)
	if !c.now().Before(ent.expires) {
		c.removeElement(el)
		c.misses.Add(1)
		return nil, false
	}
	c.ll.MoveToFront(el)
	c.hits.Add(1)
	return ent.listing, true
}

// put inserts (or refreshes) a listing under key with a fresh TTL, then evicts LRU victims until
// both the entry and byte bounds hold.
func (c *listingCache) put(key string, listing *DirListing) {
	b := listingBytes(listing)
	c.mu.Lock()
	defer c.mu.Unlock()
	exp := c.now().Add(c.ttl)
	if el, ok := c.items[key]; ok {
		ent := el.Value.(*listingCacheEntry)
		c.curBytes += b - ent.bytes
		ent.listing = listing
		ent.bytes = b
		ent.expires = exp
		c.ll.MoveToFront(el)
	} else {
		ent := &listingCacheEntry{key: key, listing: listing, bytes: b, expires: exp}
		c.items[key] = c.ll.PushFront(ent)
		c.curBytes += b
	}
	for c.ll.Len() > c.maxEntries || (c.curBytes > c.maxBytes && c.ll.Len() > 1) {
		c.removeElement(c.ll.Back())
		c.evictions.Add(1)
	}
}

// removeElement drops el from both the list and the map and decrements the byte total.
// Caller holds c.mu.
func (c *listingCache) removeElement(el *list.Element) {
	if el == nil {
		return
	}
	ent := el.Value.(*listingCacheEntry)
	c.ll.Remove(el)
	delete(c.items, ent.key)
	c.curBytes -= ent.bytes
}

// stats returns a snapshot of the best-effort counters (for the D5b observability tie-in).
func (c *listingCache) stats() (hits, misses, evictions uint64) {
	return c.hits.Load(), c.misses.Load(), c.evictions.Load()
}

// listingKey builds the cache key. generation makes a busy↔idle flip / new browse-target
// unreachable by key (free invalidation); dirPath+page scope it to one directory page. The NUL
// separators keep ("a","b") distinct from ("ab","") style concatenation ambiguities.
func listingKey(projectID, generation, dirPath string, page int) string {
	return projectID + "\x00" + generation + "\x00" + dirPath + "\x00" + strconv.Itoa(page)
}

// listingBytes approximates the heap footprint of a listing for the byte bound: a fixed per-entry
// overhead (the DirEntry struct + slice header share) plus the name bytes, and a small base for the
// listing header fields. It is an estimate, not an exact sizeof — the bound only needs to be
// monotonic and roughly proportional so a wide-tree burst cannot blow memory.
func listingBytes(l *DirListing) int {
	const perEntryOverhead = 48 // DirEntry{Name,Type,Size} + map/slice bookkeeping, rounded up
	total := 64                 // listing header: generation/reason/snapshotTakenAt strings etc.
	for _, e := range l.Entries {
		total += perEntryOverhead + len(e.Name) + len(e.Type)
	}
	return total
}
