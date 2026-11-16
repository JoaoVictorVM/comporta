package responsecache

import "time"

// Builder captures one response as it is delivered. Every byte is charged to
// the global budget before it is copied, so a capture can never grow past the
// per-entry limit or the free budget. A Builder belongs to one goroutine;
// Publish and Abort end it exactly once.
type Builder struct {
	cache       *Cache
	key         Key
	generation  int64
	epoch       uint64
	kind        Kind
	contentType string
	body        []byte
	events      [][]byte
	charged     int64
	done        bool
}

// Begin starts a capture for key in generation, or returns nil when the cache
// is closed, the generation is no longer current or there is no room even for
// its metadata. A nil Builder accepts every call and keeps nothing.
func (c *Cache) Begin(key Key, generation int64, kind Kind, contentType string) *Builder {
	if kind != KindJSON && kind != KindSSE {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.observe(generation)
	if generation != c.generation {
		return nil
	}
	overhead := entryOverhead + int64(len(key.app)+len(contentType))
	if overhead > c.entryLimit() || !c.reserve(overhead) {
		return nil
	}
	c.builders++
	return &Builder{cache: c, key: key, generation: generation, epoch: c.epochs[key.app], kind: kind, contentType: contentType, charged: overhead}
}

// Append copies one piece of the response: the whole body of a JSON answer or
// one complete event of a stream. False means the capture was abandoned, for
// lack of room or a second JSON body, and every later call is ignored.
func (b *Builder) Append(data []byte) bool {
	if b == nil {
		return false
	}
	cost := int64(len(data))
	if b.kind == KindSSE {
		cost += eventOverhead
	}
	c := b.cache
	c.mu.Lock()
	if b.done {
		c.mu.Unlock()
		return false
	}
	if (b.kind == KindJSON && b.body != nil) || b.charged+cost > c.entryLimit() || !c.reserve(cost) {
		b.abortLocked()
		c.mu.Unlock()
		return false
	}
	b.charged += cost
	c.mu.Unlock()
	// The copy happens after the reservation and outside the lock: the cache
	// never holds its mutex while moving response bytes.
	piece := make([]byte, len(data))
	copy(piece, data)
	if b.kind == KindJSON {
		b.body = piece
	} else {
		b.events = append(b.events, piece)
	}
	return true
}

// Publish indexes the capture with an expiry of ttl from now. It hands over
// the captured bytes without copying them. It fails, discarding the capture,
// when the cache closed, the generation moved on, the app was purged since
// Begin or nothing was captured.
func (b *Builder) Publish(ttl time.Duration) bool {
	if b == nil {
		return false
	}
	c := b.cache
	c.mu.Lock()
	defer c.mu.Unlock()
	if b.done {
		return false
	}
	empty := (b.kind == KindJSON && b.body == nil) || (b.kind == KindSSE && len(b.events) == 0)
	if c.closed || ttl <= 0 || empty || b.generation != c.generation || c.epochs[b.key.app] != b.epoch {
		b.abortLocked()
		return false
	}
	b.done = true
	c.builders--
	if old := c.index[b.key]; old != nil {
		c.unindex(old)
	}
	e := &entry{key: b.key, kind: b.kind, contentType: b.contentType, body: b.body, events: b.events, expires: c.now().Add(ttl), charged: b.charged, indexed: true}
	e.elem = c.lru.PushFront(e)
	c.index[b.key] = e
	b.body, b.events, b.charged = nil, nil, 0
	return true
}

// Abort discards the capture. It is idempotent and safe on a nil Builder.
func (b *Builder) Abort() {
	if b == nil {
		return
	}
	b.cache.mu.Lock()
	defer b.cache.mu.Unlock()
	b.abortLocked()
}

func (b *Builder) abortLocked() {
	if b.done {
		return
	}
	b.done = true
	c := b.cache
	c.builders--
	c.used -= b.charged
	b.body, b.events, b.charged = nil, nil, 0
}

// Active reports whether the capture can still be published.
func (b *Builder) Active() bool {
	if b == nil {
		return false
	}
	b.cache.mu.Lock()
	defer b.cache.mu.Unlock()
	return !b.done
}
