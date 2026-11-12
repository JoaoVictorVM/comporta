package metering

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/events"
	"github.com/JoaoVictorVM/comporta/internal/loopguard"
	"github.com/JoaoVictorVM/comporta/internal/proxy"
	"github.com/JoaoVictorVM/comporta/internal/storage"
)

// RecordLoopCut makes a loop cut durable before the refusal is answered: the
// record of this request and, in the same transaction, the episode event. It
// has no quote, no reservation and no usage, and never goes through Admit.
func (s *Service) RecordLoopCut(ctx context.Context, snapshot *config.Snapshot, e *proxy.Envelope, d loopguard.Decision) error {
	cuts, ok := s.store.(storage.LocalCutStore)
	if !ok || d.Allowed {
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
	now := s.now().UTC()
	status := http.StatusTooManyRequests
	hash, anonymous := e.Identity.Hash().Hex(), e.Identity.Anonymous()
	record := storage.Record{
		RequestID: e.RequestID, AttemptIndex: 0, StartedAt: now, FinishedAt: now,
		AppName: e.AppName, UserHash: hash, Anonymous: anonymous,
		Operation: string(e.Operation), ModelRequested: e.ModelRequested,
		Outcome: "loop_cut", ErrorClass: string(proxy.ClassRateLimit), ErrorCode: string(proxy.CodeLoopDetected),
		DownstreamStatus: &status, Day: now.In(location).Format("2006-01-02"), Timezone: app.Timezone,
	}
	episode := d.Episode
	event := events.NewLoopCut(episode.EventID, episode.At.Truncate(time.Microsecond), episode.RequestID, e.AppName, hash, anonymous,
		string(episode.Reason), episode.Count, episode.Window, episode.Cooldown)
	if err := cuts.CommitLocalCut(ctx, storage.LocalCut{Record: record, Event: event}); err != nil {
		if !errors.Is(err, context.Canceled) {
			s.warnStorage(err)
		}
		return err
	}
	return nil
}
