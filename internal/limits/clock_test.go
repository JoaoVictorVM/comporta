package limits

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestClockBackwardsMoreThanFiveSeconds(t *testing.T) {
	e := newEnv(t, rpmPolicy(1, 0))
	var logs bytes.Buffer
	e.manager.logger = slog.New(slog.NewTextHandler(&logs, nil))
	u := user("bot", 1)
	e.settle(e.mustReserve(e.request(u, 1, 0)), 0, 1)
	e.clock.Advance(10 * time.Second)
	e.clock.SetCivil(e.clock.Now().Add(-time.Hour))
	e.mustReject(e.request(u, 1, 0))
	e.mustReject(e.request(u, 1, 0))
	if n := strings.Count(logs.String(), "clock moved backwards"); n != 1 {
		t.Fatalf("expected one rate-limited warning, got %d", n)
	}
	// The window follows monotonic time, not the wall clock that jumped back.
	e.clock.Advance(50 * time.Second)
	e.mustReserve(e.request(u, 1, 0))
}

func TestRestartBeforeWatermark(t *testing.T) {
	e := newEnv(t, dailyUSD(100, "UTC"))
	e.clock.SetCivil(time.Date(2026, 11, 9, 0, 0, 30, 0, time.UTC))
	u := user("bot", 1)
	e.settle(e.mustReserve(e.request(u, 1, 100)), 100, 1)
	if err := e.manager.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	// The host restarts with a clock one minute behind, inside the previous day.
	e.clock.SetCivil(time.Date(2026, 11, 8, 23, 59, 30, 0, time.UTC))
	e.restart()
	d := e.mustReject(e.request(u, 1, 1))
	if d.Primary.Used != 100 {
		t.Fatalf("restart before the watermark reopened a closed window: %+v", d.Primary)
	}
}

func TestForwardJumpResetsOnce(t *testing.T) {
	e := newEnv(t, dailyUSD(100, "UTC"))
	u := user("bot", 1)
	e.settle(e.mustReserve(e.request(u, 1, 100)), 100, 1)
	e.clock.SetCivil(e.clock.Now().Add(24 * time.Hour))
	e.settle(e.mustReserve(e.request(u, 1, 100)), 100, 1)
	// Going back to the real time must not open the jumped-over day again.
	e.clock.SetCivil(e.clock.Now().Add(-24 * time.Hour))
	e.mustReject(e.request(u, 1, 1))
	e.clock.SetCivil(e.clock.Now().Add(24*time.Hour + time.Minute))
	e.mustReject(e.request(u, 1, 1))
}
