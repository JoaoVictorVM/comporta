package limits

import "time"

const slidingSpan = 60 * time.Second

// rateEvent is one admitted logical request. One event is shared by the app
// window and the user window.
type rateEvent struct {
	requestID string
	at        time.Duration // monotonic elapsed of the admission
	civil     time.Time
	tokens    int64 // estimate while active, real total after settlement
	active    bool
	settled   bool
}

// slidingWindow holds events admitted in the last 60 seconds, plus events that
// expired while still in flight. A long stream keeps its token reservation until
// it ends instead of losing it when its admission leaves the window.
type slidingWindow struct {
	events        []*rateEvent
	overdue       map[*rateEvent]struct{}
	count         int64
	tokens        int64
	activeTokens  int64
	overdueTokens int64
}

func newSlidingWindow() *slidingWindow {
	return &slidingWindow{overdue: map[*rateEvent]struct{}{}}
}

// expired reports whether an event no longer counts: the interval is open at
// its start, so an event exactly 60 s old is out.
func expired(e *rateEvent, now time.Duration) bool { return e.at <= now-slidingSpan }

func (w *slidingWindow) prune(now time.Duration) {
	drop := 0
	for _, e := range w.events {
		if !expired(e, now) {
			break
		}
		drop++
		w.count--
		w.tokens -= e.tokens
		if e.active {
			w.overdue[e] = struct{}{}
			w.overdueTokens += e.tokens
		}
	}
	if drop > 0 {
		w.events = append(w.events[:0:0], w.events[drop:]...)
	}
}

func (w *slidingWindow) add(e *rateEvent) {
	w.events = append(w.events, e)
	w.count++
	w.tokens += e.tokens
	if e.active {
		w.activeTokens += e.tokens
	}
}

// settle replaces the estimate of an event with its real total. Once settled,
// tokens belong to the admission instant, so an expired event stops counting.
func (w *slidingWindow) settle(e *rateEvent, tokens int64, wasActive bool) {
	old := e.tokens
	if wasActive {
		w.activeTokens -= old
	}
	if _, late := w.overdue[e]; late {
		delete(w.overdue, e)
		w.overdueTokens -= old
		return
	}
	// An active event is never dropped by prune, so it is still in events.
	w.tokens += tokens - old
}

func (w *slidingWindow) requests() int64 { return w.count }

func (w *slidingWindow) usedTokens() int64 { return w.tokens + w.overdueTokens }

func (w *slidingWindow) reservedTokens() int64 { return w.activeTokens }

func (w *slidingWindow) empty() bool { return len(w.events) == 0 && len(w.overdue) == 0 }

// requestRetry is how long until enough admissions leave the window for one
// more request to fit under the limit.
func (w *slidingWindow) requestRetry(limit int64, now time.Duration) time.Duration {
	need := w.count - limit + 1
	if need <= 0 || need > int64(len(w.events)) {
		return 0
	}
	return w.events[need-1].at + slidingSpan - now
}

// tokenRetry is how long until settled tokens leave the window and make room.
// Tokens still in flight have no known release time, so they never produce a
// promise: zero means no honest Retry-After exists.
func (w *slidingWindow) tokenRetry(limit, requested int64, now time.Duration) time.Duration {
	excess := w.usedTokens() + requested - limit
	if excess <= 0 || requested > limit {
		return 0
	}
	freed := int64(0)
	for _, e := range w.events {
		if e.active {
			continue
		}
		freed += e.tokens
		if freed >= excess {
			return e.at + slidingSpan - now
		}
	}
	return 0
}

func ceilSeconds(d time.Duration) int64 {
	if d <= 0 {
		return 1
	}
	seconds := int64(d / time.Second)
	if d%time.Second != 0 {
		seconds++
	}
	return max(1, seconds)
}
