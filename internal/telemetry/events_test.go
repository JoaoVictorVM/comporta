package telemetry

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/events"
)

type fixedSubscriber struct{ sub *events.Subscription }

func (f fixedSubscriber) Subscribe(context.Context, ...events.Type) *events.Subscription {
	return f.sub
}

func TestEventLossDoesNotChangeRequestCounters(t *testing.T) {
	e := newEnv(t, "")
	for range 3 {
		op := e.begin(OperationGeneration)
		op.Authenticated("bot")
		op.Route("fake-claude", "fake-claude", "anthropic", false)
		op.End(RequestEnd{Outcome: OutcomeOK, Status: Some(200)})
	}
	requests := `comporta_requests_total{app="bot",model="fake-claude",operation="generation",outcome="ok"}`

	// A real bus drops what a slow subscriber cannot take and reports it.
	bus := &events.Bus{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	saturated := bus.Subscribe(ctx)
	for range 300 {
		bus.Notify(events.New(events.Fallback, time.Now()))
	}
	if saturated.Lost.Load() == 0 {
		t.Fatal("the bus did not report a loss")
	}

	ch := make(chan events.Event, 4)
	sub := &events.Subscription{Events: ch}
	sub.Lost.Store(saturated.Lost.Load())
	tick := make(chan time.Time)
	done := make(chan error, 1)
	go func() { done <- e.service.runEvents(ctx, fixedSubscriber{sub}, tick) }()
	ch <- events.New(events.Fallback, time.Now())
	ch <- events.New(events.Cached, time.Now())
	ch <- events.Event{Type: events.Type("client text")}
	tick <- time.Now()
	deadline := time.Now().Add(5 * time.Second)
	for {
		out := e.scrape()
		if sample(out, `comporta_events_total{type="cached"}`) == "1" && sample(out, "comporta_event_notifications_lost_total") != "0" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("events not counted:\n%s", out)
		}
		time.Sleep(5 * time.Millisecond)
	}
	out := e.scrape()
	if got, want := sample(out, "comporta_event_notifications_lost_total"), strconv.FormatUint(saturated.Lost.Load(), 10); got != want {
		t.Fatalf("lost = %q, want %q", got, want)
	}
	if sample(out, `comporta_events_total{type="fallback"}`) != "1" || sample(out, `comporta_events_total{type="client text"}`) != "" {
		t.Fatalf("event types:\n%s", out)
	}
	// The main counters never come from the bus.
	if sample(out, requests) != "3" || sample(out, `comporta_fallbacks_total{app="bot",cause="503"}`) != "" || sample(out, `comporta_cache_hits_total{app="bot",model="fake-claude"}`) != "" {
		t.Fatalf("bus events changed the main counters:\n%s", out)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
