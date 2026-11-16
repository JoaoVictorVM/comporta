package responsecache

import (
	"context"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/config"
)

// sweepInterval bounds how long an expired or invalidated entry can keep its
// memory when no request touches it. Lookups and publications check expiry
// and generation themselves, so correctness never depends on this pace.
const sweepInterval = time.Second

// Settings is the part of a configuration generation the cache follows.
type Settings struct {
	Generation int64
	MaxBytes   int64
	Apps       []string
}

// FromSnapshot extracts the settings of snapshot.
func FromSnapshot(snapshot *config.Snapshot) Settings {
	return Settings{Generation: snapshot.Generation(), MaxBytes: snapshot.Cache().MaxBytes, Apps: snapshot.AppNames()}
}

// Sync applies a configuration generation. A newer one invalidates every
// entry, whatever changed; an older one is ignored. A smaller budget evicts
// unpinned entries at once, and while leases still hold more than it allows no
// new capture finds room.
func (c *Cache) Sync(s Settings) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || s.Generation < c.generation {
		return
	}
	if s.Generation > c.generation {
		c.invalidateAll()
		c.generation = s.Generation
		active := make(map[string]bool, len(s.Apps))
		for _, app := range s.Apps {
			active[app] = true
		}
		for app := range c.epochs {
			if !active[app] {
				delete(c.epochs, app)
			}
		}
	}
	c.max = capacity(s.MaxBytes)
	for c.used > c.max && c.evictOne() {
	}
}

// Run keeps the cache in step with the configuration and sweeps expired
// entries until ctx ends, then closes it.
func (c *Cache) Run(ctx context.Context, settings func() Settings) error {
	ticker := time.NewTicker(sweepInterval)
	defer ticker.Stop()
	defer c.Close()
	for {
		c.Sync(settings())
		c.Sweep()
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// Close drops every entry and refuses anything new. Leases and captures still
// running release their bytes when they end. Nothing is written anywhere.
func (c *Cache) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	c.invalidateAll()
}
