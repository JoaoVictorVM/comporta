package sqlite

import (
	"testing"
)

// BenchmarkAdmitReservedAndSettle includes the two synchronous commits, with
// synchronous=FULL, that admission and completion add to a request.
func BenchmarkAdmitReservedAndSettle(b *testing.B) {
	s := testStore(b)
	m := limitManager(b, s)
	for i := 0; b.Loop(); i++ {
		r := limitRecord(i + 1)
		h, err := admitLimited(m, s, r)
		if err != nil {
			b.Fatal(err)
		}
		if err = settleCompleted(m, s, h, completed(r)); err != nil {
			b.Fatal(err)
		}
	}
}
