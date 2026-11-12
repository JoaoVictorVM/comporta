package loopguard

import (
	"sync"
	"time"
)

// Clock separates the civil instant, used only for event metadata, from the
// monotonic elapsed time that drives windows and cooldowns. Tests replace both.
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

// FakeClock moves only when told to. Civil and monotonic time move apart with
// SetCivil, to simulate an operator or NTP step.
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
	var due []*fakeTimer
	for _, t := range c.timers {
		if t.armed && t.at <= c.elapsed {
			t.armed = false
			due = append(due, t)
		}
	}
	now := c.civil
	c.mu.Unlock()
	for _, t := range due {
		select {
		case t.ch <- now:
		default:
		}
	}
}

// SetCivil changes only the wall clock.
func (c *FakeClock) SetCivil(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.civil = t.UTC()
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
