package alerts

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/JoaoVictorVM/comporta/internal/storage"
)

func saoPaulo(string) string { return "America/Sao_Paulo" }

func samples() []Payload {
	hash := "9f2c4f8d26ee61b0904f9a0f70bd2141"
	return []Payload{
		{Type: TriggerBudget, OccurredAt: "2026-11-23T18:30:00Z", App: "bot-discord", Scope: "app", Dimension: "usd_per_day", Unit: "microusd", Used: ptr(1_600_000), Limit: ptr(2_000_000),
			ThresholdPercent: 80, WindowStart: "2026-11-23T03:00:00Z", WindowEnd: "2026-11-24T03:00:00Z", RenewsAt: "2026-11-24T03:00:00Z"},
		{Type: TriggerBudget, OccurredAt: "2026-11-23T18:30:00Z", App: "bot", Scope: "user", UserHash: hash, Dimension: "tokens_per_month", Unit: "tokens", Used: ptr(1_250_000), Limit: ptr(1_000_000),
			ThresholdPercent: 100, WindowStart: "2026-11-01T03:00:00Z", WindowEnd: "2026-12-01T03:00:00Z", RenewsAt: "2026-12-01T03:00:00Z"},
		{Type: TriggerHardCap, OccurredAt: "2026-11-23T18:30:00Z", App: "bot", Scope: "app", Dimension: "usd_per_day", Unit: "microusd", Used: ptr(3_000_000), Limit: ptr(3_000_000), RenewsAt: "2026-11-24T03:00:00Z", WindowEnd: "2026-11-24T03:00:00Z"},
		{Type: TriggerAnomaly, OccurredAt: "2026-11-23T18:30:00Z", App: "bot", Scope: "app", Unit: "microusd", Multiplier: 5, BaselineMicrousd: ptr(100_000), LastHourMicrousd: ptr(650_000)},
		{Type: TriggerLoopCut, OccurredAt: "2026-11-23T18:31:00Z", App: "bot-discord", Scope: "user", UserHash: hash, Count: 10, WindowStart: "2026-11-23T18:30:30Z", WindowEnd: "2026-11-23T18:31:00Z"},
		{Type: TriggerFallback, OccurredAt: "2026-11-23T18:31:00Z", App: "bot", Scope: "app", Count: 6},
		{Type: TriggerReloadFailed, OccurredAt: "2026-11-23T18:31:00Z", Scope: "global", Count: 2, Generation: 7},
		{Type: TriggerFailOpen, OccurredAt: "2026-11-23T18:31:00Z", App: "bot", Scope: "app", WindowStart: "2026-11-23T18:29:00Z"},
	}
}

var allowed = map[string]bool{
	"id": true, "type": true, "occurred_at": true, "app": true, "scope": true, "user_hash": true, "dimension": true, "unit": true, "used": true, "limit": true,
	"threshold_percent": true, "window_start": true, "window_end": true, "renews_at": true, "count": true, "multiplier": true,
	"baseline_microusd": true, "last_hour_microusd": true, "generation": true,
}

func TestGenericPayloadAllowlist(t *testing.T) {
	for _, p := range samples() {
		o, err := build("key|"+string(p.Type), p, occurrenceShape(p))
		if err != nil {
			t.Fatalf("%s: %v", p.Type, err)
		}
		again, _ := build("key|"+string(p.Type), p, occurrenceShape(p))
		if !bytes.Equal(o.Payload, again.Payload) {
			t.Fatalf("%s payload is not deterministic", p.Type)
		}
		var fields map[string]any
		if err := json.Unmarshal(o.Payload, &fields); err != nil {
			t.Fatal(err)
		}
		for k := range fields {
			if !allowed[k] {
				t.Fatalf("%s payload has field %q outside the allowlist", p.Type, k)
			}
		}
		if fields["id"] != o.ID || len(o.ID) != 32 {
			t.Fatalf("%s payload id %v differs from occurrence %s", p.Type, fields["id"], o.ID)
		}
	}
}

func occurrenceShape(p Payload) (o storage.AlertOccurrence) {
	o.Trigger, o.App, o.Scope, o.Dimension = string(p.Type), p.App, p.Scope, p.Dimension
	if p.Type == TriggerBudget {
		o.ThresholdPercent = p.ThresholdPercent
	}
	if p.Type == TriggerBudget || p.Type == TriggerHardCap {
		end := evaluation
		o.WindowEnd = &end
	}
	if p.UserHash != "" {
		anonymous := false
		o.UserHash, o.Anonymous = p.UserHash, &anonymous
	}
	o.OccurredAt = evaluation
	return o
}

func TestMessageFormats(t *testing.T) {
	s := samples()
	cases := map[string]string{
		ChatText(s[0], saoPaulo, DiscordMaxRunes): "⚠️ bot-discord reached 80% of its daily budget ($1.60 of $2.00). Renews at 00:00.",
		Sentence(s[1], saoPaulo):                  "bot user u_9f2c4f… reached 100% of their monthly token budget (1,250,000 tokens of 1,000,000 tokens). Renews on Dec 1 at 00:00.",
		Sentence(s[2], nil):                       "bot hit its daily hard cap ($3.00 of $3.00); requests are blocked. Renews at 03:00.",
		Sentence(s[3], nil):                       "unusual spend in bot: $0.65 in the last hour, more than 5x its 7-day hourly average ($0.10)",
		Sentence(s[5], nil):                       "fallback burst in bot: 6 fallbacks in 5 minutes",
		Sentence(s[6], nil):                       "configuration reload rejected with 2 problems; generation 7 stays active",
		Sentence(s[7], nil):                       "bot is admitting requests without verified limits (fail_open) since 18:29 UTC",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("got  %q\nwant %q", got, want)
		}
	}
	var ntfy struct {
		Topic, Title, Message string
		Priority              int
		Tags                  []string
	}
	if err := json.Unmarshal(NtfyBody(s[4], nil, "comporta-alerts"), &ntfy); err != nil {
		t.Fatal(err)
	}
	if ntfy.Message != "Comporta: loop cut in bot-discord (user u_9f2c4f…, 10 repeats in 30s)" || ntfy.Topic != "comporta-alerts" || ntfy.Title != "Comporta alert" || ntfy.Priority != 4 {
		t.Fatalf("ntfy body %+v", ntfy)
	}
	var discord map[string]any
	if err := json.Unmarshal(DiscordBody(s[0], saoPaulo), &discord); err != nil || discord["allowed_mentions"] == nil {
		t.Fatalf("discord body %v", discord)
	}
	for _, p := range s {
		if text := Sentence(p, nil); strings.Contains(text, p.UserHash) && p.UserHash != "" {
			t.Fatalf("message carries the full user hash: %s", text)
		}
	}
}

func TestMessageLengthBounds(t *testing.T) {
	long := strings.Repeat("é", 5000)
	if got := truncate(long, DiscordMaxRunes); utf8.RuneCountInString(got) != DiscordMaxRunes || !strings.HasSuffix(got, "…") || !utf8.ValidString(got) {
		t.Fatal("truncation broke the rune bound")
	}
	if got := truncate("short", DiscordMaxRunes); got != "short" {
		t.Fatal("short text changed")
	}
	for _, p := range samples() {
		o, err := build("key", p, occurrenceShape(p))
		if err != nil || len(o.Payload) > 16<<10 {
			t.Fatal("payload over 16 KiB")
		}
		var d struct{ Content string }
		var sl struct{ Text string }
		var n struct{ Message string }
		_ = json.Unmarshal(DiscordBody(p, nil), &d)
		_ = json.Unmarshal(SlackBody(p, nil), &sl)
		_ = json.Unmarshal(NtfyBody(p, nil, "t"), &n)
		if utf8.RuneCountInString(d.Content) > DiscordMaxRunes || utf8.RuneCountInString(sl.Text) > SlackMaxRunes || utf8.RuneCountInString(n.Message) > NtfyMaxRunes || d.Content == "" {
			t.Fatalf("%s message out of bounds", p.Type)
		}
	}
}
