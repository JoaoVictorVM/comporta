package metering

import (
	"context"
	"errors"
	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/events"
	"log/slog"
	"sync"
	"time"
)

type ConfigEvents struct {
	Publisher events.Publisher
	Logger    *slog.Logger
	once      sync.Once
	queue     chan events.Event
}

func (s *ConfigEvents) init() { s.once.Do(func() { s.queue = make(chan events.Event, 256) }) }

func (s *ConfigEvents) Publish(ctx context.Context, change config.Event) error {
	s.init()
	kind := events.ConfigChanged
	result := config.ResultApplied
	if len(change.Errors) > 0 {
		kind = events.ConfigReloadFailed
		result = config.ResultRejected
	}
	e := events.New(kind, change.At)
	e.Payload.Generation = change.Generation
	e.Payload.Result = result
	e.Payload.Changes = append([]config.Change(nil), change.Changes...)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case s.queue <- e.Clone():
		return nil
	default:
		return errors.New("configuration event queue full")
	}
}

func (s *ConfigEvents) Run(ctx context.Context) error {
	s.init()
	var pending *events.Event
	var lastWarning time.Time
	for {
		if ctx.Err() != nil {
			return s.drain(pending)
		}
		if pending == nil {
			select {
			case <-ctx.Done():
				return s.drain(nil)
			case e := <-s.queue:
				pending = &e
			}
		}
		attempt, cancel := context.WithTimeout(context.Background(), time.Second)
		err := s.Publisher.Publish(attempt, *pending)
		cancel()
		if err == nil {
			pending = nil
			continue
		}
		if s.Logger != nil && (lastWarning.IsZero() || time.Since(lastWarning) >= 30*time.Second) {
			s.Logger.Error("configuration event persistence unavailable")
			lastWarning = time.Now()
		}
		tick := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			tick.Stop()
			return s.drain(pending)
		case <-tick.C:
		}
	}
}

func (s *ConfigEvents) drain(pending *events.Event) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if pending != nil {
		if err := s.Publisher.Publish(ctx, *pending); err != nil {
			return err
		}
	}
	for {
		select {
		case e := <-s.queue:
			if err := s.Publisher.Publish(ctx, e); err != nil {
				return err
			}
		default:
			return nil
		}
	}
}
