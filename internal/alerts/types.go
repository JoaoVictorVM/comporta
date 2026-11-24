// Package alerts derives deduplicated alert occurrences from durable events and
// financial state, and delivers each one to every configured channel outside the
// request path. The event bus is only a wake-up: the inbox, the occurrences and
// the deliveries live in the store, so a lost notification or a restart never
// loses or repeats an alert.
package alerts

import (
	"cmp"
	"encoding/json"
	"slices"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/storage"
)

// Trigger is the closed set of alert causes.
type Trigger string

const (
	TriggerBudget       Trigger = "budget_threshold"
	TriggerHardCap      Trigger = "hard_cap"
	TriggerAnomaly      Trigger = "anomaly"
	TriggerLoopCut      Trigger = "loop_cut"
	TriggerFallback     Trigger = "fallback_burst"
	TriggerReloadFailed Trigger = "config_reload_failed"
	TriggerFailOpen     Trigger = "fail_open"
)

// Rules that the product defines rather than the configuration.
const (
	DedupeWindow       = 15 * time.Minute
	FallbackBurstSize  = 5
	FallbackBurstSpan  = 5 * time.Minute
	AnomalyStep        = 5 * time.Minute
	AnomalyRecent      = time.Hour
	AnomalyBaseline    = 7 * 24 * time.Hour
	AnomalyBaselineHrs = 168
)

// Payload is the canonical projection of an occurrence and the exact body of
// the generic webhook. Every field is metadata: names from validated
// configuration, closed categories, numbers and times. It never carries
// content, a raw user ID, a URL, a secret or an external error.
type Payload struct {
	ID               string  `json:"id"`
	Type             Trigger `json:"type"`
	OccurredAt       string  `json:"occurred_at"`
	App              string  `json:"app,omitempty"`
	Scope            string  `json:"scope"`
	UserHash         string  `json:"user_hash,omitempty"`
	Dimension        string  `json:"dimension,omitempty"`
	Unit             string  `json:"unit,omitempty"`
	Used             *int64  `json:"used,omitempty"`
	Limit            *int64  `json:"limit,omitempty"`
	ThresholdPercent int     `json:"threshold_percent,omitempty"`
	WindowStart      string  `json:"window_start,omitempty"`
	WindowEnd        string  `json:"window_end,omitempty"`
	RenewsAt         string  `json:"renews_at,omitempty"`
	Count            int64   `json:"count,omitempty"`
	Multiplier       int64   `json:"multiplier,omitempty"`
	BaselineMicrousd *int64  `json:"baseline_microusd,omitempty"`
	LastHourMicrousd *int64  `json:"last_hour_microusd,omitempty"`
	Generation       int64   `json:"generation,omitempty"`
}

// DecodePayload reads a stored projection back.
func DecodePayload(data []byte) (Payload, error) {
	var p Payload
	if err := json.Unmarshal(data, &p); err != nil {
		return p, storage.ErrInvalid
	}
	return p, nil
}

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func ptr(v int64) *int64 { return &v }

// quarter is the UTC fifteen minute bucket that deduplicates every trigger
// without a budget window.
func quarter(t time.Time) time.Time { return t.UTC().Truncate(DedupeWindow) }

// unitOf maps a budget dimension to the unit of its values.
func unitOf(dimension string) string {
	if dimension == "usd_per_day" || dimension == "usd_per_month" {
		return "microusd"
	}
	return "tokens"
}

// ceiling reads one dimension of a budget or hard cap.
func ceiling(b config.Budget, dimension string) int64 {
	switch dimension {
	case "usd_per_day":
		return int64(b.USDPerDay)
	case "usd_per_month":
		return int64(b.USDPerMonth)
	case "tokens_per_day":
		return b.TokensPerDay
	case "tokens_per_month":
		return b.TokensPerMonth
	}
	return 0
}

// ChannelRefs lists the configured channels in a stable order.
func ChannelRefs(alerts config.Alerts) []storage.AlertChannelRef {
	refs := make([]storage.AlertChannelRef, 0, len(alerts.Channels))
	for name, channel := range alerts.Channels {
		refs = append(refs, storage.AlertChannelRef{Name: name, Type: string(channel.Type)})
	}
	slices.SortFunc(refs, func(a, b storage.AlertChannelRef) int { return cmp.Compare(a.Name, b.Name) })
	return refs
}
