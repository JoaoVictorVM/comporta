package limits

import (
	"fmt"
	"log/slog"
	"testing"

	"github.com/JoaoVictorVM/comporta/internal/config"
)

// BenchmarkReserveSettle measures the coordinator alone, without disk.
func BenchmarkReserveSettle(b *testing.B) {
	store := newMemStore()
	m := New(Options{Store: store, Clock: NewFakeClock(start), Logger: slog.New(slog.DiscardHandler)})
	p := Policy{App: "bot", Timezone: "America/Sao_Paulo", AppSet: Set{Limits: config.Limits{RequestsPerMinute: 1 << 40, MaxConcurrent: 1 << 20}, Budget: config.Budget{USDPerDay: 1 << 50}},
		UserSet: Set{Budget: config.Budget{USDPerDay: 1 << 50, TokensPerMonth: 1 << 50}}}
	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		r := Request{RequestID: fmt.Sprintf("%032x", i+1), AttemptIndex: 1, Subject: Subject{App: "bot", Hash: fmt.Sprintf("%032x", i%1000+1)}, Operation: OperationGeneration, EstimatedTokens: 4000, ReservedCost: 25000}
		h, d, err := m.Reserve(p, r, store.commit)
		if err != nil || !d.Allowed {
			b.Fatal(err)
		}
		if err = m.Settle(h, Usage{Cost: 20000, Tokens: 3000, Complete: true}, nil); err != nil {
			b.Fatal(err)
		}
	}
}
