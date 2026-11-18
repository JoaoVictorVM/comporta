package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"log/slog"
	"net/netip"
	"time"
)

// Limiter counts failed authentications per connection address.
type Limiter interface {
	Admit(addr netip.Addr) Decision
	Failure(addr netip.Addr) Decision
}

var _ Limiter = (*Guard)(nil)

// SharedWindow is the failure history of one origin, as instants in micros.
type SharedWindow struct {
	Failures     []int64
	BlockedUntil int64
}

// SharedWindows is the database side of the shared guard. Update holds a lock
// on the origin while fn runs, so failures from several replicas all count.
type SharedWindows interface {
	Read(ctx context.Context, surface string, origin [32]byte) (SharedWindow, error)
	Update(ctx context.Context, surface string, origin [32]byte, expires time.Time, fn func(SharedWindow) (SharedWindow, bool)) (SharedWindow, error)
}

// SharedGuard applies the thresholds of a Guard to a count shared by every
// replica. Addresses never reach the database: only an HMAC with a key derived
// for this purpose. It does not depend on Redis, so authentication keeps
// working while financial limits run degraded; a failure of its own storage
// refuses the attempt instead of letting it through uncounted.
type SharedGuard struct {
	windows     SharedWindows
	surface     string
	key         []byte
	clock       Clock
	logger      *slog.Logger
	maxFailures int
	window      time.Duration
	cooldown    time.Duration
	timeout     time.Duration
}

// NewSharedGuard returns a guard for one surface, "proxy" or "admin".
func NewSharedGuard(windows SharedWindows, surface string, key []byte, logger *slog.Logger, options GuardOptions) *SharedGuard {
	if options.MaxFailures <= 0 {
		options.MaxFailures = guardMaxFailures
	}
	if options.Window <= 0 {
		options.Window = guardWindow
	}
	if options.Cooldown <= 0 {
		options.Cooldown = guardCooldown
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &SharedGuard{windows: windows, surface: surface, key: key, clock: systemClock{}, logger: logger,
		maxFailures: options.MaxFailures, window: options.Window, cooldown: options.Cooldown, timeout: 500 * time.Millisecond}
}

func (g *SharedGuard) origin(addr netip.Addr) [32]byte {
	mac := hmac.New(sha256.New, g.key)
	mac.Write([]byte(g.surface + "\x00"))
	mac.Write(addr.Unmap().AsSlice())
	var out [32]byte
	copy(out[:], mac.Sum(nil))
	return out
}

func (g *SharedGuard) refuse() Decision {
	g.logger.Warn("authentication protection storage unavailable; attempt refused")
	return Decision{RetryAfter: time.Second}
}

func (g *SharedGuard) Admit(addr netip.Addr) Decision {
	ctx, cancel := context.WithTimeout(context.Background(), g.timeout)
	defer cancel()
	w, err := g.windows.Read(ctx, g.surface, g.origin(addr))
	if err != nil {
		return g.refuse()
	}
	now := g.clock.Now().UTC().UnixMicro()
	if w.BlockedUntil > now {
		return Decision{RetryAfter: time.Duration(w.BlockedUntil-now) * time.Microsecond}
	}
	return Decision{Allowed: true}
}

func (g *SharedGuard) Failure(addr netip.Addr) Decision {
	ctx, cancel := context.WithTimeout(context.Background(), g.timeout)
	defer cancel()
	now := g.clock.Now().UTC()
	at := now.UnixMicro()
	var decision Decision
	_, err := g.windows.Update(ctx, g.surface, g.origin(addr), now.Add(g.window+g.cooldown), func(w SharedWindow) (SharedWindow, bool) {
		if w.BlockedUntil > at {
			decision = Decision{RetryAfter: time.Duration(w.BlockedUntil-at) * time.Microsecond}
			return w, false
		}
		kept := w.Failures[:0]
		for _, f := range w.Failures {
			if at-f < g.window.Microseconds() {
				kept = append(kept, f)
			}
		}
		kept = append(kept, at)
		next := SharedWindow{Failures: kept}
		if len(kept) >= g.maxFailures {
			next = SharedWindow{Failures: []int64{}, BlockedUntil: now.Add(g.cooldown).UnixMicro()}
			g.logger.Info("authentication protection", "blocks_started", 1)
		}
		// History is bounded by the threshold: the window never needs more.
		if len(next.Failures) > g.maxFailures {
			next.Failures = next.Failures[len(next.Failures)-g.maxFailures:]
		}
		decision = Decision{Allowed: true}
		return next, true
	})
	if err != nil {
		return g.refuse()
	}
	return decision
}
