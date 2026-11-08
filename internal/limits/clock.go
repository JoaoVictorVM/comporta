package limits

import (
	"sync"
	"time"
)

// Clock separates the civil instant, used by calendar budgets, from the
// monotonic elapsed time, used by sliding windows. Tests replace both.
type Clock interface {
	Now() time.Time
	Elapsed() time.Duration
	NewTimer(time.Duration) Timer
}

type Timer interface {
	C() <-chan time.Time
	Reset(time.Duration)
	Stop()
}

type systemClock struct{ origin time.Time }

// SystemClock reads the wall clock and Go's monotonic reading.
func SystemClock() Clock { return systemClock{origin: time.Now()} }

func (c systemClock) Now() time.Time                 { return time.Now().Round(0).UTC() }
func (c systemClock) Elapsed() time.Duration         { return time.Since(c.origin) }
func (c systemClock) NewTimer(d time.Duration) Timer { return systemTimer{time.NewTimer(d)} }

type systemTimer struct{ t *time.Timer }

func (t systemTimer) C() <-chan time.Time   { return t.t.C }
func (t systemTimer) Reset(d time.Duration) { t.t.Reset(d) }
func (t systemTimer) Stop()                 { t.t.Stop() }

// FakeClock moves only when told to. Civil and monotonic time can be moved
// independently to simulate clock adjustments.
type FakeClock struct {
	mu      sync.Mutex
	civil   time.Time
	elapsed time.Duration
	timers  []*fakeTimer
}

func NewFakeClock(civil time.Time) *FakeClock { return &FakeClock{civil: civil.UTC()} }

func (c *FakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.civil
}

func (c *FakeClock) Elapsed() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.elapsed
}

// Advance moves civil and monotonic time together and fires due timers.
func (c *FakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.civil = c.civil.Add(d)
	c.elapsed += d
	due := c.dueLocked()
	c.mu.Unlock()
	for _, t := range due {
		select {
		case t.ch <- c.Now():
		default:
		}
	}
}

// SetCivil changes only the wall clock, as an operator or NTP step would.
func (c *FakeClock) SetCivil(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.civil = t.UTC()
}

func (c *FakeClock) dueLocked() []*fakeTimer {
	var due []*fakeTimer
	for _, t := range c.timers {
		if t.armed && t.at <= c.elapsed {
			t.armed = false
			due = append(due, t)
		}
	}
	return due
}

func (c *FakeClock) NewTimer(d time.Duration) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{clock: c, ch: make(chan time.Time, 1), at: c.elapsed + d, armed: true}
	c.timers = append(c.timers, t)
	return t
}

type fakeTimer struct {
	clock *FakeClock
	ch    chan time.Time
	at    time.Duration
	armed bool
}

func (t *fakeTimer) C() <-chan time.Time { return t.ch }

func (t *fakeTimer) Reset(d time.Duration) {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	t.at = t.clock.elapsed + d
	t.armed = true
}

func (t *fakeTimer) Stop() {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	t.armed = false
}

const (
	backwardsTolerance = 5 * time.Second
	warningInterval    = 30 * time.Second
)

// civilGuard turns the wall clock into a non-decreasing effective instant. A
// clock that steps back never reopens a closed calendar window; it only freezes
// the effective instant until the wall clock catches up.
type civilGuard struct {
	effective   time.Time
	lastCivil   time.Time
	lastElapsed time.Duration
	started     bool
	lastWarning time.Duration
	warned      bool
}

// observe returns the effective civil instant and whether a backwards step
// worth a warning was seen.
func (g *civilGuard) observe(civil time.Time, elapsed time.Duration) (time.Time, bool) {
	warn := false
	if g.started {
		monotonic := elapsed - g.lastElapsed
		wall := civil.Sub(g.lastCivil)
		if monotonic-wall > backwardsTolerance && (!g.warned || elapsed-g.lastWarning >= warningInterval) {
			warn = true
			g.warned = true
			g.lastWarning = elapsed
		}
	}
	g.started = true
	g.lastCivil, g.lastElapsed = civil, elapsed
	if civil.After(g.effective) {
		g.effective = civil
	}
	return g.effective, warn
}

// restore starts from a persisted watermark, so a restart with an earlier
// clock does not renew a budget ahead of time.
func (g *civilGuard) restore(watermark time.Time) {
	if watermark.After(g.effective) {
		g.effective = watermark.UTC()
	}
}
