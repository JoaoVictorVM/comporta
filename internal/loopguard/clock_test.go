package loopguard

import (
	"testing"
	"time"
)

func TestWallClockJumpDoesNotChangeCooldown(t *testing.T) {
	e := newEnv(t, Options{})
	u := user("agent", 1)
	for range 10 {
		e.allow(u, "same", defaultPolicy())
	}
	e.cut(u, "same", defaultPolicy())
	e.clock.SetCivil(start.Add(24 * time.Hour))
	if d := e.cut(u, "same", defaultPolicy()); d.Remaining != 120*time.Second {
		t.Fatalf("a civil jump forward ended the cooldown: %+v", d)
	}
	e.clock.SetCivil(start.Add(-24 * time.Hour))
	e.clock.Advance(120 * time.Second)
	e.allow(u, "same", defaultPolicy())
}

func TestFakeTimerFires(t *testing.T) {
	c := NewFakeClock(start)
	timer := c.NewTimer(time.Second)
	c.Advance(999 * time.Millisecond)
	select {
	case <-timer.C():
		t.Fatal("fired early")
	default:
	}
	c.Advance(time.Millisecond)
	select {
	case <-timer.C():
	default:
		t.Fatal("did not fire")
	}
	timer.Reset(time.Second)
	timer.Stop()
	c.Advance(time.Hour)
	select {
	case <-timer.C():
		t.Fatal("stopped timer fired")
	default:
	}
}
