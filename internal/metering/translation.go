package metering

import (
	"context"
	"errors"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/events"
	"github.com/JoaoVictorVM/comporta/internal/proxy"
	"github.com/JoaoVictorVM/comporta/internal/storage"
)

// RecordTranslationSkip makes a skipped model durable before the next one is
// considered. It writes one idempotent event and nothing else: no attempt,
// reservation, rate event or journal entry. The same store holds it in single
// and distributed mode, and a replica repeating the write confirms the same
// event.
func (s *Service) RecordTranslationSkip(ctx context.Context, e *proxy.Envelope, skip proxy.TranslationSkip) error {
	publisher, ok := s.store.(events.Publisher)
	if !ok {
		return storage.ErrUnavailable
	}
	event := events.NewTranslationSkip(skip.At.Truncate(time.Microsecond), e.RequestID, e.AppName, skip.Position, skip.Model,
		string(skip.Source), string(skip.Target), string(skip.Feature))
	if err := event.ValidateTranslationSkip(); err != nil {
		return storage.ErrInvalid
	}
	if err := publisher.Publish(ctx, event); err != nil {
		if !errors.Is(err, context.Canceled) {
			s.warnStorage(err)
		}
		return err
	}
	return nil
}
