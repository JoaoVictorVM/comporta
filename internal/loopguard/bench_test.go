package loopguard

import (
	"fmt"
	"testing"
)

// BenchmarkCheckDistinctUsers measures the stage alone, without I/O.
func BenchmarkCheckDistinctUsers(b *testing.B) {
	clock := NewFakeClock(start)
	m := New(Options{Clock: clock})
	p := defaultPolicy()
	subjects := make([]Subject, 1000)
	for i := range subjects {
		subjects[i] = Subject{App: "bench", Hash: fmt.Sprintf("%032x", i+1)}
	}
	signature := sig("prompt")
	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		if i%1000 == 0 {
			clock.Advance(RateWindow)
		}
		if _, err := m.Check(Request{Subject: subjects[i%len(subjects)], Signature: signature, HasSignature: true, Policy: p}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkCheckRepeatedSignature includes the cut and the purge of a
// subject that keeps repeating itself.
func BenchmarkCheckRepeatedSignature(b *testing.B) {
	clock := NewFakeClock(start)
	m := New(Options{Clock: clock})
	p := defaultPolicy()
	subject := Subject{App: "bench", Hash: fmt.Sprintf("%032x", 1)}
	signature := sig("prompt")
	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		if i%11 == 0 {
			clock.Advance(p.Cooldown)
		}
		if _, err := m.Check(Request{Subject: subject, Signature: signature, HasSignature: true, Policy: p}); err != nil {
			b.Fatal(err)
		}
	}
}
