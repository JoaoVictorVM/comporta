package telemetry

import (
	"context"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/events"
)

// knownEvents is the closed set counted by events_total. Anything else is
// ignored, so a label never comes from a payload.
var knownEvents = set(events.RequestCompleted, events.Blocked, events.Cached, events.Fallback, events.Downgraded, events.Warned,
	events.LoopCut, events.ConfigChanged, events.ConfigReloadFailed, events.Paused, events.SensitiveDataDetected,
	events.LimitStateUnavailable, events.LimitStateRecovered)

// Subscriber is the event bus as seen by the diagnostics.
type Subscriber interface {
	Subscribe(ctx context.Context, types ...events.Type) *events.Subscription
}

// RunEvents counts the operational events this process receives from now on,
// by type only: no payload, ID or hash is kept. Notifications the bus could
// not deliver are counted from the subscription's loss counter. These are
// diagnostics: requests, consumption, cache and fallback counters never
// depend on them.
func (s *Service) RunEvents(ctx context.Context, bus Subscriber) error {
	return s.runEvents(ctx, bus, time.NewTicker(budgetRefresh).C)
}

func (s *Service) runEvents(ctx context.Context, bus Subscriber, tick <-chan time.Time) error {
	sub := bus.Subscribe(ctx)
	var lost uint64
	account := func() {
		if current := sub.Lost.Load(); current > lost {
			if s.metricsEnabled.Load() {
				s.f.eventsLost.add(int64(current - lost))
			}
			lost = current
		}
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case e, ok := <-sub.Events:
			if !ok {
				return nil
			}
			if knownEvents[e.Type] && s.metricsEnabled.Load() {
				s.f.events.add(1, string(e.Type))
			}
			account()
		case <-tick:
			account()
		}
	}
}
