package limits

import (
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/config"
)

func rpmPolicy(app, user int64) Policy {
	return Policy{AppSet: Set{Limits: config.Limits{RequestsPerMinute: app}}, UserSet: Set{Limits: config.Limits{RequestsPerMinute: user}}}
}

func TestSixtyFirstRequestWithinSlidingMinuteRejected(t *testing.T) {
	for _, name := range []string{"sequential", "burst-across-civil-minute"} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, rpmPolicy(60, 0))
			u := user("bot", 1)
			if name == "burst-across-civil-minute" {
				// 30 calls in the last 500 ms of one civil minute and 30 in the first
				// 500 ms of the next: a fixed-minute bucket would allow 120.
				e.clock.Advance(59500 * time.Millisecond)
			}
			for i := range 60 {
				h := e.mustReserve(e.request(u, 1, 0))
				e.settle(h, 0, 1)
				if name == "sequential" {
					e.clock.Advance(500 * time.Millisecond)
				} else if i == 29 {
					e.clock.Advance(time.Second)
				}
			}
			d := e.mustReject(e.request(u, 1, 0))
			if d.Primary.Kind != KindRate || d.Primary.Dimension != RequestsPerMinute || d.Primary.LimitName != "apps.bot.limits.requests_per_minute" || d.Primary.RetryAfter < 1 {
				t.Fatalf("61st request: %+v", d.Primary)
			}
		})
	}
}

func TestExactSixtySecondBoundary(t *testing.T) {
	e := newEnv(t, rpmPolicy(1, 0))
	u := user("bot", 1)
	e.settle(e.mustReserve(e.request(u, 1, 0)), 0, 1)
	e.clock.Advance(59999 * time.Millisecond)
	d := e.mustReject(e.request(u, 1, 0))
	if d.Primary.RetryAfter != 1 {
		t.Fatalf("retry after %d", d.Primary.RetryAfter)
	}
	e.clock.Advance(time.Millisecond)
	e.mustReserve(e.request(u, 1, 0))
}

func TestTokenReservationSettlesAtAdmissionTime(t *testing.T) {
	e := newEnv(t, Policy{AppSet: Set{Limits: config.Limits{TokensPerMinute: 1000}}})
	u := user("bot", 1)
	h := e.mustReserve(e.request(u, 800, 0))
	d := e.mustReject(e.request(u, 300, 0))
	if d.Primary.Reason != ReasonReservation || d.Primary.RetryAfter != 0 {
		t.Fatalf("in-flight tokens must block as reservation without a promise: %+v", d.Primary)
	}
	e.clock.Advance(30 * time.Second)
	e.settle(h, 0, 200)
	e.mustReserve(e.request(u, 300, 0))
	// The 200 real tokens belong to the first admission and leave with it.
	e.clock.Advance(30 * time.Second)
	if v := e.view(nil, "apps.bot.limits.tokens_per_minute"); v.Used+v.Reserved != 300 {
		t.Fatalf("tokens at admission time: %+v", v)
	}
}

func TestLongStreamKeepsTokenReservation(t *testing.T) {
	e := newEnv(t, Policy{AppSet: Set{Limits: config.Limits{TokensPerMinute: 1000}}})
	u := user("bot", 1)
	h := e.mustReserve(e.request(u, 900, 0))
	e.clock.Advance(5 * time.Minute)
	if d := e.mustReject(e.request(u, 200, 0)); d.Primary.Reason != ReasonReservation {
		t.Fatalf("expired admission of a live stream lost its reservation: %+v", d.Primary)
	}
	e.settle(h, 0, 900)
	e.mustReserve(e.request(u, 200, 0))
}
