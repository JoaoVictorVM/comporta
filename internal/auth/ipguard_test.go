package auth

import (
	"bytes"
	"log/slog"
	"net/netip"
	"strings"
	"testing"
	"time"
)

var testAddr = netip.MustParseAddr("192.0.2.10")

func failTimes(t *testing.T, guard *Guard, addr netip.Addr, times int) {
	t.Helper()
	for i := range times {
		if decision := guard.Failure(addr); !decision.Allowed {
			t.Fatalf("failure %d was refused", i+1)
		}
	}
}

func TestTwentyFirstAttemptIsBlocked(t *testing.T) {
	clock := newFakeClock()
	guard := NewGuard(clock, nil)
	for i := range 20 {
		if decision := guard.Admit(testAddr); !decision.Allowed {
			t.Fatalf("attempt %d must be verified", i+1)
		}
		if decision := guard.Failure(testAddr); !decision.Allowed {
			t.Fatalf("failure %d must get the ordinary answer", i+1)
		}
	}
	decision := guard.Admit(testAddr)
	if decision.Allowed {
		t.Fatal("attempt 21 must be blocked")
	}
	if decision.RetryAfterSeconds() != 300 {
		t.Fatalf("retry after %d", decision.RetryAfterSeconds())
	}
	if other := guard.Admit(netip.MustParseAddr("192.0.2.11")); !other.Allowed {
		t.Fatal("other addresses are unaffected")
	}
}

func TestValidKeyCannotBypassIPBlock(t *testing.T) {
	guard := NewGuard(newFakeClock(), nil)
	failTimes(t, guard, testAddr, 20)
	if guard.Admit(testAddr).Allowed {
		t.Fatal("verification must not even start during a block")
	}
}

func TestFailureWindowBoundary(t *testing.T) {
	clock := newFakeClock()
	guard := NewGuard(clock, nil)
	failTimes(t, guard, testAddr, 19)

	clock.advance(60*time.Second - time.Nanosecond)
	failTimes(t, guard, testAddr, 1)
	if guard.Admit(testAddr).Allowed {
		t.Fatal("20 failures inside the window must block")
	}

	clock = newFakeClock()
	guard = NewGuard(clock, nil)
	failTimes(t, guard, testAddr, 19)
	clock.advance(60 * time.Second)
	failTimes(t, guard, testAddr, 1)
	if !guard.Admit(testAddr).Allowed {
		t.Fatal("at exactly 60 s the earlier failures have left the window")
	}
}

func TestSuccessDoesNotClearFailures(t *testing.T) {
	guard := NewGuard(newFakeClock(), nil)
	failTimes(t, guard, testAddr, 19)
	if !guard.Admit(testAddr).Allowed {
		t.Fatal("still below the limit")
	}
	failTimes(t, guard, testAddr, 1)
	if guard.Admit(testAddr).Allowed {
		t.Fatal("a success in between must not have reset the count")
	}
}

func TestCooldownExpiresWithoutExtension(t *testing.T) {
	clock := newFakeClock()
	guard := NewGuard(clock, nil)
	failTimes(t, guard, testAddr, 20)

	clock.advance(100 * time.Second)
	decision := guard.Admit(testAddr)
	if decision.Allowed || decision.RetryAfterSeconds() != 200 {
		t.Fatalf("retry after %d", decision.RetryAfterSeconds())
	}
	clock.advance(100*time.Second - 500*time.Millisecond)
	if got := guard.Admit(testAddr).RetryAfterSeconds(); got != 101 {
		t.Fatalf("seconds must round up, got %d", got)
	}
	clock.advance(100*time.Second + 500*time.Millisecond - time.Nanosecond)
	if guard.Admit(testAddr).Allowed {
		t.Fatal("one nanosecond before the end it is still blocked")
	}
	clock.advance(time.Nanosecond)
	if !guard.Admit(testAddr).Allowed {
		t.Fatal("attempts during the block must not extend it")
	}

	failTimes(t, guard, testAddr, 19)
	if !guard.Admit(testAddr).Allowed {
		t.Fatal("after a block the count starts over")
	}
}

func TestMappedIPv4SharesCounter(t *testing.T) {
	guard := NewGuard(newFakeClock(), nil)
	plain, ok := connectionAddress(requestFrom("192.0.2.10:1000"))
	if !ok {
		t.Fatal("plain address")
	}
	mapped, ok := connectionAddress(requestFrom("[::ffff:192.0.2.10]:2000"))
	if !ok {
		t.Fatal("mapped address")
	}
	if plain != mapped {
		t.Fatalf("%v != %v", plain, mapped)
	}
	failTimes(t, guard, plain, 10)
	failTimes(t, guard, mapped, 10)
	if guard.Admit(plain).Allowed {
		t.Fatal("both forms must share one counter")
	}
}

func addrNumber(i int) netip.Addr {
	return netip.AddrFrom4([4]byte{10, byte(i >> 16), byte(i >> 8), byte(i)})
}

func TestGuardCapacityDoesNotEvictBlockedIP(t *testing.T) {
	clock := newFakeClock()
	guard := NewGuard(clock, nil)
	guard.capacity = 3
	blocked := addrNumber(1)
	failTimes(t, guard, blocked, 20)
	failTimes(t, guard, addrNumber(2), 1)
	failTimes(t, guard, addrNumber(3), 1)

	clock.advance(2 * time.Second)
	fresh := addrNumber(4)
	decision := guard.Admit(fresh)
	if decision.Allowed || decision.RetryAfterSeconds() != 60 {
		t.Fatalf("a full table must refuse a new address: %+v", decision)
	}
	if guard.Failure(fresh).Allowed {
		t.Fatal("an attempt that cannot be counted must be refused")
	}
	if guard.Admit(blocked).Allowed {
		t.Fatal("the block must survive saturation")
	}
	if !guard.Admit(addrNumber(2)).Allowed {
		t.Fatal("known addresses keep working")
	}

	clock.advance(61 * time.Second)
	if !guard.Admit(fresh).Allowed {
		t.Fatal("idle entries must expire before a new address is admitted")
	}
	if guard.Admit(blocked).Allowed {
		t.Fatal("expiring idle entries must keep the blocked one")
	}
	if len(guard.entries) != 1 {
		t.Fatalf("entries left: %d", len(guard.entries))
	}
}

func TestGuardMemoryIsBounded(t *testing.T) {
	clock := newFakeClock()
	guard := NewGuard(clock, nil)
	guard.capacity = 50
	for i := range 500 {
		guard.Failure(addrNumber(i))
	}
	if len(guard.entries) > 50 {
		t.Fatalf("entries %d", len(guard.entries))
	}
	known := addrNumber(0)
	for i := range 40 {
		guard.Failure(known)
		if got := len(guard.entries[known].failures); got > 20 {
			t.Fatalf("attempt %d kept %d times", i, got)
		}
	}
	if guardMaxAddresses != 100_000 {
		t.Fatal("documented capacity changed")
	}
}

func TestGuardLogsOnlyAggregates(t *testing.T) {
	var logs bytes.Buffer
	clock := newFakeClock()
	guard := NewGuard(clock, slog.New(slog.NewJSONHandler(&logs, nil)))
	failTimes(t, guard, testAddr, 20)
	if logs.Len() != 0 {
		t.Fatalf("no log per attempt: %s", logs.String())
	}
	clock.advance(guardSweepEvery)
	guard.Admit(addrNumber(99))
	clock.advance(guardCooldown)
	guard.Admit(addrNumber(99))
	text := logs.String()
	if !strings.Contains(text, `"blocks_started":1`) || !strings.Contains(text, `"blocks_ended":1`) {
		t.Fatalf("aggregates: %s", text)
	}
	if strings.Contains(text, testAddr.String()) {
		t.Fatalf("logs expose an address: %s", text)
	}
}
