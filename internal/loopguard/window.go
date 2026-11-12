package loopguard

import "time"

// arrivals is a time-ordered queue of monotonic instants. Windows are open on
// the left: an instant belongs to the window while it is later than now minus
// the window, so an arrival exactly one window ago has already expired.
type arrivals struct {
	items []time.Duration
	head  int
}

func (q *arrivals) len() int { return len(q.items) - q.head }

func (q *arrivals) push(at time.Duration) { q.items = append(q.items, at) }

// purge drops the instants at or before cutoff and returns how many left.
func (q *arrivals) purge(cutoff time.Duration) int {
	start := q.head
	for q.head < len(q.items) && q.items[q.head] <= cutoff {
		q.head++
	}
	removed := q.head - start
	q.compact()
	return removed
}

func (q *arrivals) oldest() (time.Duration, bool) {
	if q.len() == 0 {
		return 0, false
	}
	return q.items[q.head], true
}

func (q *arrivals) reset() int {
	n := q.len()
	q.items, q.head = nil, 0
	return n
}

func (q *arrivals) compact() {
	switch {
	case q.head == len(q.items):
		q.items, q.head = q.items[:0], 0
	case q.head > 32 && q.head*2 >= len(q.items):
		n := copy(q.items, q.items[q.head:])
		q.items, q.head = q.items[:n], 0
	}
}

type signed struct {
	at     time.Duration
	digest [32]byte
}

// signatures counts each digest seen inside the similar window. Expired slots
// are zeroed, so a digest is not kept past its window even in a reused array.
type signatures struct {
	items  []signed
	head   int
	counts map[[32]byte]int64
}

func (q *signatures) len() int { return len(q.items) - q.head }

func (q *signatures) count(digest [32]byte) int64 { return q.counts[digest] }

func (q *signatures) push(at time.Duration, digest [32]byte) {
	if q.counts == nil {
		q.counts = map[[32]byte]int64{}
	}
	q.items = append(q.items, signed{at: at, digest: digest})
	q.counts[digest]++
}

func (q *signatures) purge(cutoff time.Duration) int {
	start := q.head
	for q.head < len(q.items) && q.items[q.head].at <= cutoff {
		digest := q.items[q.head].digest
		if q.counts[digest]--; q.counts[digest] <= 0 {
			delete(q.counts, digest)
		}
		q.items[q.head] = signed{}
		q.head++
	}
	removed := q.head - start
	switch {
	case q.head == len(q.items):
		q.items, q.head = q.items[:0], 0
		if len(q.counts) == 0 {
			q.counts = nil
		}
	case q.head > 32 && q.head*2 >= len(q.items):
		n := copy(q.items, q.items[q.head:])
		clear(q.items[n:])
		q.items, q.head = q.items[:n], 0
	}
	return removed
}

func (q *signatures) oldest() (time.Duration, bool) {
	if q.len() == 0 {
		return 0, false
	}
	return q.items[q.head].at, true
}

func (q *signatures) reset() int {
	n := q.len()
	clear(q.items)
	q.items, q.head, q.counts = nil, 0, nil
	return n
}
