package responsecache

import (
	"container/list"
	"sync"
	"time"
)

// entry is an immutable stored response. Its bytes stay charged to the budget
// until it left the index and its last lease ended.
type entry struct {
	key         Key
	kind        Kind
	contentType string
	body        []byte
	events      [][]byte
	expires     time.Time
	charged     int64
	refs        int
	indexed     bool
	elem        *list.Element
}

// Options configures a Cache.
type Options struct {
	// MaxBytes is the global budget; zero or out of range means MaxCapacity.
	MaxBytes int64
	// Now replaces the clock in tests.
	Now func() time.Time
}

// Cache is a global LRU with absolute expiry. Every method is safe for
// concurrent use and never performs I/O while holding its lock.
type Cache struct {
	mu         sync.Mutex
	now        func() time.Time
	max        int64
	used       int64
	index      map[Key]*entry
	lru        *list.List
	generation int64
	// epochs advance when an app is purged, so a capture started earlier can
	// never publish into it again.
	epochs   map[string]uint64
	leases   int
	builders int
	closed   bool
}

// New returns an empty cache.
func New(options Options) *Cache {
	now := options.Now
	if now == nil {
		now = time.Now
	}
	return &Cache{now: now, max: capacity(options.MaxBytes), index: map[Key]*entry{}, lru: list.New(), epochs: map[string]uint64{}}
}

func capacity(maxBytes int64) int64 {
	if maxBytes <= 0 || maxBytes > MaxCapacity {
		return MaxCapacity
	}
	return maxBytes
}

func (c *Cache) entryLimit() int64 { return min(MaxEntryBytes, c.max) }

// Lease is a read-only view of a stored response for one replay. The bytes it
// returns must not be modified nor retained after Release.
type Lease struct {
	cache    *Cache
	entry    *entry
	released bool
}

func (l *Lease) Kind() Kind          { return l.entry.kind }
func (l *Lease) ContentType() string { return l.entry.contentType }
func (l *Lease) Body() []byte        { return l.entry.body }
func (l *Lease) Events() [][]byte    { return l.entry.events }

// Release ends the lease. It is idempotent and safe on a nil lease.
func (l *Lease) Release() {
	if l == nil {
		return
	}
	c := l.cache
	c.mu.Lock()
	defer c.mu.Unlock()
	if l.released {
		return
	}
	l.released = true
	c.leases--
	l.entry.refs--
	if !l.entry.indexed && l.entry.refs == 0 {
		c.free(l.entry)
	}
}

// Lookup returns a lease on the valid entry of key in generation, or false.
// A hit refreshes recency but never the expiry.
func (c *Cache) Lookup(key Key, generation int64) (*Lease, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, false
	}
	c.observe(generation)
	if generation != c.generation {
		return nil, false
	}
	e := c.index[key]
	if e == nil {
		return nil, false
	}
	if !c.now().Before(e.expires) {
		c.unindex(e)
		return nil, false
	}
	c.lru.MoveToFront(e.elem)
	e.refs++
	c.leases++
	return &Lease{cache: c, entry: e}, true
}

// Invalidate removes key, for an entry found unusable before anything was sent.
func (c *Cache) Invalidate(key Key) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e := c.index[key]; e != nil {
		c.unindex(e)
	}
}

// observe advances to a newer generation seen by a request before the worker
// did. An older one never moves the cache back.
func (c *Cache) observe(generation int64) {
	if generation > c.generation {
		c.invalidateAll()
		c.generation = generation
	}
}

func (c *Cache) invalidateAll() {
	for _, e := range c.index {
		c.unindex(e)
	}
}

func (c *Cache) unindex(e *entry) {
	if !e.indexed {
		return
	}
	delete(c.index, e.key)
	c.lru.Remove(e.elem)
	e.indexed, e.elem = false, nil
	if e.refs == 0 {
		c.free(e)
	}
}

func (c *Cache) free(e *entry) {
	c.used -= e.charged
	e.charged, e.body, e.events = 0, nil, nil
}

// reserve charges n bytes, evicting unpinned entries from the least recently
// used end when needed. Pinned entries keep their bytes until released.
func (c *Cache) reserve(n int64) bool {
	if n < 0 || n > c.max {
		return false
	}
	for c.used+n > c.max {
		if !c.evictOne() {
			return false
		}
	}
	c.used += n
	return true
}

func (c *Cache) evictOne() bool {
	for elem := c.lru.Back(); elem != nil; elem = elem.Prev() {
		e := elem.Value.(*entry)
		if e.refs == 0 {
			c.unindex(e)
			return true
		}
	}
	return false
}

// PurgeApps removes every entry of apps, shared ones included, and advances
// their epochs even when nothing was stored, so captures already running for
// those apps are discarded instead of published.
func (c *Cache) PurgeApps(apps []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	purge := make(map[string]bool, len(apps))
	for _, app := range apps {
		purge[app] = true
		c.epochs[app]++
	}
	for _, e := range c.index {
		if purge[e.key.app] {
			c.unindex(e)
		}
	}
}

// Sweep removes expired entries.
func (c *Cache) Sweep() {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	for _, e := range c.index {
		if !now.Before(e.expires) {
			c.unindex(e)
		}
	}
}

// Stats returns safe counters.
func (c *Cache) Stats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return Stats{Entries: len(c.index), UsedBytes: c.used, MaxBytes: c.max, Leases: c.leases, Builders: c.builders}
}
