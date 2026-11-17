package telemetry

import (
	"context"
	"slices"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/config"
)

const (
	// shutdownLimit bounds the whole telemetry shutdown.
	shutdownLimit = 5 * time.Second
	// maxProviders is the current provider plus one draining.
	maxProviders = 2
)

// Reconcile applies the telemetry settings of a snapshot once per
// generation. Requests already observing keep the provider they began with;
// only new requests see the change. An older generation never undoes a newer
// one.
func (s *Service) Reconcile(snapshot *config.Snapshot) {
	if s == nil || snapshot == nil {
		return
	}
	generation := snapshot.Generation()
	s.mu.Lock()
	if s.closed || generation <= s.reconciled {
		s.mu.Unlock()
		return
	}
	s.reconciled = generation
	settings := snapshot.Telemetry()
	s.metricsEnabled.Store(settings.MetricsEnabled)
	want := ""
	if settings.OTelEnabled {
		want = settings.OTelEndpoint
	}
	if s.current != nil && s.current.endpoint == want {
		s.mu.Unlock()
		return
	}
	var retired []*traceProvider
	if old := s.current; old != nil {
		s.current = nil
		if old.sessions == 0 {
			retired = append(retired, old)
		} else {
			old.draining = true
			s.draining = append(s.draining, old)
		}
	}
	var failed bool
	if want != "" {
		provider, err := s.newProvider(want)
		if err != nil {
			failed = true
		} else {
			s.current = provider
		}
	}
	// A further generation retires the oldest draining provider: its open
	// sessions stop producing spans, so reloads never pile up exporters.
	for len(s.draining)+s.active() > maxProviders {
		oldest := s.draining[0]
		s.draining = s.draining[1:]
		oldest.retired.Store(true)
		retired = append(retired, oldest)
	}
	s.mu.Unlock()
	if failed {
		s.diagnose(ComponentReload, ReasonProviderInit)
	}
	for _, p := range retired {
		reason := DropShutdownTimeout
		if p.draining {
			reason = DropReloadRetired
			s.diagnose(ComponentReload, ReasonRetired)
		}
		s.shutdownProvider(p, reason)
	}
}

func (s *Service) active() int {
	if s.current != nil {
		return 1
	}
	return 0
}

// Providers reports how many tracer providers exist, for tests.
func (s *Service) Providers() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active() + len(s.draining)
}

func (s *Service) removeDraining(p *traceProvider) {
	s.draining = slices.DeleteFunc(s.draining, func(x *traceProvider) bool { return x == p })
}

// shutdownProvider stops a provider in the background within the shutdown
// limit; spans ending after it are counted with reason.
func (s *Service) shutdownProvider(p *traceProvider, reason string) {
	if !p.stopping.CompareAndSwap(false, true) {
		return
	}
	p.queue.halt(reason)
	s.shutdowns.Go(func() {
		ctx, cancel := context.WithTimeout(context.Background(), shutdownLimit)
		defer cancel()
		if err := p.tp.Shutdown(ctx); err != nil {
			s.diagnose(ComponentTracing, ReasonShutdownTimeout)
		}
	})
}

// Run keeps the budget gauges fresh and the configuration reconciled until
// ctx ends. It is a supervised worker.
func (s *Service) Run(ctx context.Context) error {
	return s.run(ctx, time.NewTicker(budgetRefresh).C)
}

func (s *Service) run(ctx context.Context, tick <-chan time.Time) error {
	s.RefreshBudgets()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick:
			if s.source != nil {
				s.Reconcile(s.source.Capture())
			}
			s.RefreshBudgets()
		}
	}
}

// Close stops every provider and waits for their exports within the
// shutdown limit, on success, failure or interruption alike. It is safe to
// call more than once.
func (s *Service) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	providers := append([]*traceProvider(nil), s.draining...)
	if s.current != nil {
		providers = append(providers, s.current)
	}
	s.current, s.draining = nil, nil
	s.mu.Unlock()
	for _, p := range providers {
		s.shutdownProvider(p, DropShutdownTimeout)
	}
	done := make(chan struct{})
	go func() {
		s.shutdowns.Wait()
		close(done)
	}()
	timer := time.NewTimer(shutdownLimit + time.Second)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		s.diagnose(ComponentTracing, ReasonShutdownTimeout)
	}
}
