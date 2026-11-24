package cli

import (
	"context"
	"log/slog"

	"github.com/JoaoVictorVM/comporta/internal/alerts"
	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/events"
	"github.com/JoaoVictorVM/comporta/internal/panel"
	"github.com/JoaoVictorVM/comporta/internal/storage"
	"github.com/JoaoVictorVM/comporta/internal/telemetry"
)

// newAlerts builds the alert worker of either deployment mode and exposes its
// counters and backlog on the metrics of the admin listener.
func newAlerts(store storage.AlertStore, manager *config.Manager, bus *events.Bus, logger *slog.Logger, observer *telemetry.Service) *alerts.Service {
	metrics := telemetry.NewAlertMetrics()
	_ = observer.Register(metrics)
	return alerts.New(alerts.Options{Store: store, Source: manager, Bus: bus, Logger: logger, Metrics: metrics})
}

// alertReader feeds the panel with the most recent occurrences, rendered by
// the same fixed templates as the chat messages.
type alertReader struct {
	store  storage.AlertStore
	source alerts.ConfigSource
}

func (r alertReader) RecentAlerts(ctx context.Context, limit int) ([]panel.Alert, error) {
	occurrences, err := r.store.RecentAlertOccurrences(ctx, limit)
	if err != nil {
		return nil, err
	}
	zones := alerts.SnapshotZones(r.source.Capture())
	out := make([]panel.Alert, 0, len(occurrences))
	for _, o := range occurrences {
		p, err := alerts.DecodePayload(o.Payload)
		if err != nil {
			continue
		}
		out = append(out, panel.Alert{At: o.OccurredAt, Severity: alerts.Severity(p), Title: alerts.Sentence(p, zones), App: o.App})
	}
	return out, nil
}
