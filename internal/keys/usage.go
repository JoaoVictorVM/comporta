package keys

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

const (
	usageFlushInterval = 5 * time.Second
	usageFinalFlush    = 5 * time.Second
	usageWarnInterval  = 30 * time.Second
)

// usageTracker groups successful authentications in memory, because writing the
// document on every request would contend with administrative changes. The time is
// an approximate metadata: a crash can lose what is still pending.
type usageTracker struct {
	store  *Store
	clock  Clock
	logger *slog.Logger

	mu       sync.Mutex
	pending  map[string]time.Time
	lastWarn time.Time
}

func newUsageTracker(store *Store, clock Clock, logger *slog.Logger) *usageTracker {
	return &usageTracker{store: store, clock: clock, logger: logger, pending: make(map[string]time.Time)}
}

func (u *usageTracker) record(id string, at time.Time) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if current, ok := u.pending[id]; !ok || at.After(current) {
		u.pending[id] = at
	}
}

func (u *usageTracker) capture() map[string]time.Time {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.pending) == 0 {
		return nil
	}
	captured := make(map[string]time.Time, len(u.pending))
	for id, at := range u.pending {
		captured[id] = at
	}
	return captured
}

// confirm removes only what was saved. A newer success recorded during the flush
// has a different time and stays pending for the next one.
func (u *usageTracker) confirm(saved map[string]time.Time) {
	u.mu.Lock()
	defer u.mu.Unlock()
	for id, at := range saved {
		if u.pending[id].Equal(at) {
			delete(u.pending, id)
		}
	}
}

// flush never affects authentication: on failure everything stays pending and the
// next interval tries again. Pending entries are bounded by the number of records.
func (u *usageTracker) flush(ctx context.Context) error {
	captured := u.capture()
	if captured == nil {
		return nil
	}
	if err := u.store.mergeLastUsed(ctx, captured); err != nil {
		u.warn()
		return err
	}
	u.confirm(captured)
	return nil
}

func (u *usageTracker) warn() {
	u.mu.Lock()
	now := u.clock.Now()
	due := u.lastWarn.IsZero() || now.Sub(u.lastWarn) >= usageWarnInterval
	if due {
		u.lastWarn = now
	}
	u.mu.Unlock()
	if due {
		u.logger.Warn("key usage times could not be saved; retrying")
	}
}

func (u *usageTracker) run(ctx context.Context) {
	ticker := time.NewTicker(usageFlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			final, cancel := context.WithTimeout(context.WithoutCancel(ctx), usageFinalFlush)
			defer cancel()
			_ = u.flush(final)
			return
		case <-ticker.C:
			_ = u.flush(ctx)
		}
	}
}
