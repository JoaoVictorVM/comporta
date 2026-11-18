//go:build distributed

package distributed

import (
	"context"
	"testing"

	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/limits"
)

// BenchmarkDistributedAdmission measures one admission and its settlement on
// one app, the serialized path: gate, Redis read and apply, SQL commit, then
// the settlement transaction and its projection.
func BenchmarkDistributedAdmission(b *testing.B) {
	c := newCluster(b, 1)
	r := c.replicas[0]
	p := policy(config.Microusd(1)<<50, config.FailClosed)
	b.ResetTimer()
	for b.Loop() {
		a, rec, d, err := r.admit(b, p, 1)
		if err != nil || !d.Allowed {
			b.Fatal(err)
		}
		if err := r.coord.SettleAttempt(context.Background(), a, limits.Usage{Cost: 1, Complete: true}, r.persistFor(rec)); err != nil {
			b.Fatal(err)
		}
	}
}
