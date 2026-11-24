package alerts

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/storage"
)

// BenchmarkEventEvaluation measures one completion against budgets with a
// representative ledger: the cost of the common input.
func BenchmarkEventEvaluation(b *testing.B) {
	e := newEnv(b, defaults, nil)
	for i := range 200 {
		e.spend(fmt.Sprintf("%032x", i+1), 100)
	}
	inputs, err := e.store.ClaimAlertInputs(context.Background(), e.clock.Now(), ClaimLease, storage.MaxAlertBatch)
	if err != nil || len(inputs) == 0 {
		b.Fatal(err)
	}
	snapshot := e.manager.Capture()
	b.ResetTimer()
	for i := range b.N {
		if _, err := e.service.evaluator.Input(context.Background(), snapshot, inputs[i%len(inputs)]); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkDeliveryClaim measures a claim over an outbox of many channels.
func BenchmarkDeliveryClaim(b *testing.B) {
	e := newEnv(b, defaults, nil)
	ctx := context.Background()
	var channels []storage.AlertChannelRef
	for i := range 20 {
		channels = append(channels, storage.AlertChannelRef{Name: fmt.Sprintf("hook-%d", i), Type: "webhook"})
	}
	now := e.clock.Now()
	for i := range 200 {
		o := storage.AlertOccurrence{DedupeKey: fmt.Sprintf("loop|bot|%d", i), Trigger: "loop_cut", OccurredAt: now, App: "bot", Scope: "app", Payload: []byte(`{"type":"loop_cut"}`)}
		o.ID = storage.OccurrenceID(o.DedupeKey)
		if err := e.store.RecordAlertOccurrences(ctx, []storage.AlertOccurrence{o}, channels, now); err != nil {
			b.Fatal(err)
		}
	}
	b.ResetTimer()
	for i := range b.N {
		claimed, err := e.store.ClaimAlertDeliveries(ctx, now.Add(time.Duration(i)*time.Minute), ClaimLease, MaxConcurrent)
		if err != nil || len(claimed) == 0 {
			b.Fatal(err)
		}
	}
}

// BenchmarkAnomalyScan measures the rule over eight days of buckets for many
// apps, without the store.
func BenchmarkAnomalyScan(b *testing.B) {
	e := newEnv(b, defaults, nil)
	var buckets []storage.UsageBucket
	for _, app := range []string{"bot", "strict"} {
		for i := range 8 * 24 * 12 {
			buckets = append(buckets, storage.UsageBucket{Start: evaluation.Add(-time.Duration(i+1) * AnomalyStep), App: app, Cost: 1000})
		}
	}
	evaluator := Evaluator{Store: usageStore{buckets: buckets}}
	snapshot := e.manager.Capture()
	b.ResetTimer()
	for range b.N {
		if _, err := evaluator.Anomalies(context.Background(), snapshot, evaluation); err != nil {
			b.Fatal(err)
		}
	}
}
