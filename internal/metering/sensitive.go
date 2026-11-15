package metering

import (
	"context"
	"errors"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/events"
	"github.com/JoaoVictorVM/comporta/internal/proxy"
	"github.com/JoaoVictorVM/comporta/internal/sensitivedata"
	"github.com/JoaoVictorVM/comporta/internal/storage"
)

// PublishSensitive makes the inspection event durable before the request goes on.
// The event is derived from the request ID, so a repeated write is the same event.
func (s *Service) PublishSensitive(ctx context.Context, e *proxy.Envelope, at time.Time, summary sensitivedata.Summary) error {
	publisher, ok := s.store.(events.Publisher)
	if !ok {
		return storage.ErrUnavailable
	}
	event := events.NewSensitiveDetection(at.Truncate(time.Microsecond), e.RequestID, e.AppName, e.Identity.Hash().Hex(), e.Identity.Anonymous(), summary)
	if err := event.ValidateSensitive(); err != nil {
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

// RecordSensitiveCut makes a refusal of the inspection durable before it is
// answered. A refusal by policy carries the summary and its event in the same
// transaction; a failed scan carries neither, since its counts are incomplete. It
// has no quote, reservation or usage, and never goes through Admit.
func (s *Service) RecordSensitiveCut(ctx context.Context, snapshot *config.Snapshot, e *proxy.Envelope, cut proxy.SensitiveCut) error {
	cuts, ok := s.store.(storage.SensitiveCutStore)
	if !ok {
		return storage.ErrUnavailable
	}
	app, ok := snapshot.App(e.AppName)
	if !ok {
		return storage.ErrInvalid
	}
	location, err := time.LoadLocation(app.Timezone)
	if err != nil {
		return storage.ErrInvalid
	}
	at := cut.At.UTC().Truncate(time.Microsecond)
	status := cut.Status
	hash, anonymous := e.Identity.Hash().Hex(), e.Identity.Anonymous()
	record := storage.Record{
		RequestID: e.RequestID, AttemptIndex: 0, StartedAt: at, FinishedAt: at,
		AppName: e.AppName, UserHash: hash, Anonymous: anonymous,
		Operation: string(e.Operation), ModelRequested: e.ModelRequested,
		Outcome: "blocked", ErrorClass: string(cut.Class), ErrorCode: string(cut.Code),
		DownstreamStatus: &status, Day: at.In(location).Format("2006-01-02"), Timezone: app.Timezone,
		Detections: cut.Summary.Clone(),
	}
	commit := storage.SensitiveCut{Record: record}
	if len(cut.Summary) > 0 {
		event := events.NewSensitiveDetection(at, e.RequestID, e.AppName, hash, anonymous, cut.Summary)
		commit.Event = &event
	}
	if err := cuts.CommitSensitiveCut(ctx, commit); err != nil {
		if !errors.Is(err, context.Canceled) {
			s.warnStorage(err)
		}
		return err
	}
	return nil
}
