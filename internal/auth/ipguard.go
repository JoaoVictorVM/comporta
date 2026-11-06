package auth

import (
	"log/slog"
	"math"
	"net/netip"
	"sync"
	"time"
)

const (
	guardMaxFailures   = 20
	guardWindow        = 60 * time.Second
	guardCooldown      = 300 * time.Second
	guardMaxAddresses  = 100_000
	guardSaturationTry = 60 * time.Second
	guardSweepEvery    = 60 * time.Second
	guardFullSweepWait = 1 * time.Second
)

// Decision is the guard's answer about one address.
type Decision struct {
	Allowed    bool
	RetryAfter time.Duration
}

// RetryAfterSeconds rounds up, so a client that waits exactly this long is allowed.
func (d Decision) RetryAfterSeconds() int {
	return int(math.Ceil(d.RetryAfter.Seconds()))
}

type guardEntry struct {
	failures     []time.Time
	blockedUntil time.Time
}

// Guard limits failed authentications per connection address. Its state lives in
// this process only and a restart clears it, so it is not a distributed limit. It
// keeps no record of addresses outside memory and logs only aggregates.
type Guard struct {
	clock  Clock
	logger *slog.Logger

	capacity int

	mu            sync.Mutex
	entries       map[netip.Addr]*guardEntry
	lastSweep     time.Time
	blocksStarted int
	blocksEnded   int
}

// NewGuard returns an empty guard. A nil logger discards the aggregates.
func NewGuard(clock Clock, logger *slog.Logger) *Guard {
	if clock == nil {
		clock = systemClock{}
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Guard{clock: clock, logger: logger, capacity: guardMaxAddresses, entries: make(map[netip.Addr]*guardEntry)}
}

// Admit decides whether an attempt from this address may be verified at all. A
// blocked address is refused even with a valid key, and refusals never extend the
// block. When the table is full of entries still needed, an unseen address is
// refused too: its failures could not be counted, and evicting a blocked address
// would hand attackers a way to clear their own block.
func (g *Guard) Admit(addr netip.Addr) Decision {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.clock.Now()
	g.maybeSweep(now, guardSweepEvery)

	entry, known := g.entries[addr]
	if known {
		return g.current(entry, now)
	}
	if len(g.entries) >= g.capacity {
		g.maybeSweep(now, guardFullSweepWait)
		if len(g.entries) >= g.capacity {
			return Decision{RetryAfter: guardSaturationTry}
		}
	}
	return Decision{Allowed: true}
}

// current also ends an expired block, and a block expires at exactly its last
// instant, when verification is allowed again.
func (g *Guard) current(entry *guardEntry, now time.Time) Decision {
	if entry.blockedUntil.IsZero() {
		return Decision{Allowed: true}
	}
	if remaining := entry.blockedUntil.Sub(now); remaining > 0 {
		return Decision{RetryAfter: remaining}
	}
	entry.blockedUntil = time.Time{}
	entry.failures = entry.failures[:0]
	g.blocksEnded++
	return Decision{Allowed: true}
}

// Failure counts a failed attempt. The attempt that reaches the limit still gets
// the ordinary answer and arms the block for the ones after it. Success never
// clears the window: otherwise a key an attacker owns would reset the count.
func (g *Guard) Failure(addr netip.Addr) Decision {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.clock.Now()

	entry, known := g.entries[addr]
	if !known {
		if len(g.entries) >= g.capacity {
			g.maybeSweep(now, guardFullSweepWait)
			if len(g.entries) >= g.capacity {
				return Decision{RetryAfter: guardSaturationTry}
			}
		}
		entry = &guardEntry{}
		g.entries[addr] = entry
	}
	if decision := g.current(entry, now); !decision.Allowed {
		return decision
	}
	entry.failures = pruneFailures(entry.failures, now)
	entry.failures = append(entry.failures, now)
	if len(entry.failures) >= guardMaxFailures {
		entry.blockedUntil = now.Add(guardCooldown)
		entry.failures = entry.failures[:0]
		g.blocksStarted++
	}
	return Decision{Allowed: true}
}

// pruneFailures drops a failure at exactly the end of its window.
func pruneFailures(failures []time.Time, now time.Time) []time.Time {
	kept := failures[:0]
	for _, at := range failures {
		if now.Sub(at) < guardWindow {
			kept = append(kept, at)
		}
	}
	return kept
}

// maybeSweep expires idle entries. Throttling keeps a flood of unseen addresses
// from turning a full table into a full scan per request.
func (g *Guard) maybeSweep(now time.Time, every time.Duration) {
	if !g.lastSweep.IsZero() && now.Sub(g.lastSweep) < every {
		return
	}
	g.lastSweep = now
	for addr, entry := range g.entries {
		if !entry.blockedUntil.IsZero() {
			if entry.blockedUntil.After(now) {
				continue
			}
			g.blocksEnded++
			delete(g.entries, addr)
			continue
		}
		entry.failures = pruneFailures(entry.failures, now)
		if len(entry.failures) == 0 {
			delete(g.entries, addr)
		}
	}
	if g.blocksStarted > 0 || g.blocksEnded > 0 {
		g.logger.Info("authentication protection", "blocks_started", g.blocksStarted, "blocks_ended", g.blocksEnded)
		g.blocksStarted, g.blocksEnded = 0, 0
	}
}
