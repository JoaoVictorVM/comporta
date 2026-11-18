package telemetry

import (
	"context"
	"io"
	"strconv"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

type discardExporter struct{}

func (discardExporter) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error { return nil }
func (discardExporter) Shutdown(context.Context) error                             { return nil }

func benchEnv(b *testing.B, telemetry string) *env {
	b.Helper()
	return newEnv(b, telemetry, func(o *Options) {
		o.Exporter = func(string) (sdktrace.SpanExporter, error) { return discardExporter{}, nil }
		o.Now = time.Now
	})
}

// observeRequest plays the observations of one successful generation, the
// same calls the proxy makes, without any network.
func observeRequest(s *Service, e *env, n int) {
	op := s.Begin(e.manager.Capture(), OperationGeneration)
	for _, stage := range []Stage{StageAuthenticate, StageValidate, StageIdentity, StagePause, StageRoute, StageAdmit} {
		op.Stage(stage).End()
	}
	op.Authenticated("bot")
	op.Route("fake-claude", "fake-claude", "anthropic", true)
	id := strconv.Itoa(n)
	attempt := op.StartAttempt(id, 1, "anthropic", "fake-claude")
	deliver := op.StageAttempt(StageDeliver, 1)
	op.Delivered(time.Millisecond)
	settle := op.StageAttempt(StageSettle, 1)
	s.ObserveConsumption(Consumption{App: "bot", Provider: "anthropic", Model: "fake-claude", Operation: OperationGeneration, CostMicrousd: 1200,
		InputTokens: Some[int64](100), OutputTokens: Some[int64](30), UsageComplete: true, SettlementConfirmed: true, RequestID: id, AttemptIndex: 1})
	settle.End()
	attempt.Finish(AttemptEnd{Sent: true, Result: AttemptOK, Status: Some(200), InputTokens: Some[int64](100), OutputTokens: Some[int64](30)})
	deliver.End()
	op.End(RequestEnd{Outcome: OutcomeOK, Status: Some(200)})
}

// BenchmarkObservation compares the cost of observing a request with
// telemetry absent, metrics only, and metrics plus tracing.
func BenchmarkObservation(b *testing.B) {
	for _, tc := range []struct {
		name, telemetry string
		absent          bool
	}{
		{"absent", "", true},
		{"metrics", "", false},
		{"metrics+tracing", otelOn, false},
	} {
		b.Run(tc.name, func(b *testing.B) {
			e := benchEnv(b, tc.telemetry)
			s := e.service
			if tc.absent {
				s = nil
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := range b.N {
				observeRequest(s, e, i)
			}
		})
	}
}

// BenchmarkScrapeConcurrentTraffic scrapes while requests are observed in
// parallel, the contention a busy gateway sees.
func BenchmarkScrapeConcurrentTraffic(b *testing.B) {
	e := benchEnv(b, otelOn)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for n := 0; ctx.Err() == nil; n++ {
			observeRequest(e.service, e, n)
		}
	}()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if err := e.service.WriteMetrics(io.Discard); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	cancel()
	<-done
}
