package alerts

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/JoaoVictorVM/comporta/internal/limits"
)

// Length ceilings of each destination. Discord rejects longer content; Slack
// and ntfy accept more, but a short message is what fits a phone screen.
const (
	DiscordMaxRunes = 2000
	SlackMaxRunes   = 3000
	NtfyMaxRunes    = 3000
	ntfyTitle       = "Comporta alert"
)

// Rendering context: the timezone each app's budget renews in. A missing app
// renders in UTC.
type Zones func(app string) string

// Sentence is the fixed template of an occurrence, built only from validated
// names, closed categories, numbers and times.
func Sentence(p Payload, zones Zones) string {
	switch p.Type {
	case TriggerBudget:
		return fmt.Sprintf("%s reached %d%% of %s %s budget (%s of %s). %s", subject(p), p.ThresholdPercent, possessive(p), period(p.Dimension),
			amount(p.Dimension, p.Used), amount(p.Dimension, p.Limit), renewal(p, zones))
	case TriggerHardCap:
		used := ""
		if p.Used != nil {
			used = amount(p.Dimension, p.Used) + " of "
		}
		return fmt.Sprintf("%s hit %s %s hard cap (%s%s); requests are blocked. %s", subject(p), possessive(p), period(p.Dimension), used, amount(p.Dimension, p.Limit), renewal(p, zones))
	case TriggerAnomaly:
		return fmt.Sprintf("unusual spend in %s: %s in the last hour, more than %dx its 7-day hourly average (%s)", p.App,
			usd(p.LastHourMicrousd), p.Multiplier, usd(p.BaselineMicrousd))
	case TriggerLoopCut:
		who := ""
		if p.UserHash != "" {
			who = "user " + ShortHash(p.UserHash) + ", "
		}
		return fmt.Sprintf("loop cut in %s (%s%d repeats in %s)", p.App, who, p.Count, span(p.WindowStart, p.WindowEnd))
	case TriggerFallback:
		return fmt.Sprintf("fallback burst in %s: %d fallbacks in 5 minutes", p.App, p.Count)
	case TriggerReloadFailed:
		return fmt.Sprintf("configuration reload rejected with %d %s; generation %d stays active", p.Count, plural(p.Count, "problem", "problems"), p.Generation)
	case TriggerFailOpen:
		since := ""
		if t, err := time.Parse(time.RFC3339, p.WindowStart); err == nil {
			since = " since " + t.UTC().Format("15:04") + " UTC"
		}
		return fmt.Sprintf("%s is admitting requests without verified limits (fail_open)%s", p.App, since)
	}
	return "alert " + string(p.Type)
}

// Severity of an occurrence for the panel and the ntfy priority.
func Severity(p Payload) string {
	switch p.Type {
	case TriggerHardCap, TriggerFailOpen:
		return "critical"
	case TriggerBudget:
		if p.ThresholdPercent >= 100 {
			return "critical"
		}
	}
	return "warning"
}

// ShortHash shows a pseudonym by a short prefix: enough to correlate in the
// panel, never the full value in a chat message.
func ShortHash(hash string) string {
	if len(hash) < 6 {
		return "u_…"
	}
	return "u_" + hash[:6] + "…"
}

func subject(p Payload) string {
	if p.Scope == "user" && p.UserHash != "" {
		return p.App + " user " + ShortHash(p.UserHash)
	}
	return p.App
}

func possessive(p Payload) string {
	if p.Scope == "user" {
		return "their"
	}
	return "its"
}

func period(dimension string) string {
	switch dimension {
	case "usd_per_day":
		return "daily"
	case "usd_per_month":
		return "monthly"
	case "tokens_per_day":
		return "daily token"
	case "tokens_per_month":
		return "monthly token"
	}
	return dimension
}

func amount(dimension string, v *int64) string {
	if v == nil {
		return "?"
	}
	if unitOf(dimension) == "microusd" {
		return limits.FormatUSD(*v)
	}
	return grouped(*v) + " tokens"
}

func usd(v *int64) string {
	if v == nil {
		return "?"
	}
	return limits.FormatUSD(*v)
}

func grouped(v int64) string {
	s := strconv.FormatInt(v, 10)
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// renewal shows when the window ends in the budget's own timezone: the time
// alone for a daily window, the date too for a monthly one.
func renewal(p Payload, zones Zones) string {
	end, err := time.Parse(time.RFC3339, p.RenewsAt)
	if err != nil {
		return ""
	}
	location := time.UTC
	if zones != nil {
		if loaded, err := time.LoadLocation(zones(p.App)); err == nil {
			location = loaded
		}
	}
	local := end.In(location)
	if strings.HasSuffix(p.Dimension, "_month") {
		return "Renews on " + local.Format("Jan 2") + " at " + local.Format("15:04") + "."
	}
	return "Renews at " + local.Format("15:04") + "."
}

func span(start, end string) string {
	a, errA := time.Parse(time.RFC3339, start)
	b, errB := time.Parse(time.RFC3339, end)
	if errA != nil || errB != nil || !b.After(a) {
		return "a short window"
	}
	d := b.Sub(a)
	if d < time.Minute {
		return strconv.FormatInt(int64(d/time.Second), 10) + "s"
	}
	return strconv.FormatInt(int64(d/time.Minute), 10) + "m"
}

func plural(n int64, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// truncate cuts at a rune boundary with an ellipsis. Only generated sentences
// pass here, and every number and identifier sits before the descriptive tail.
func truncate(s string, runes int) string {
	if utf8.RuneCountInString(s) <= runes {
		return s
	}
	cut := []rune(s)[:runes-1]
	return string(cut) + "…"
}

// ChatText is the message of Discord and Slack.
func ChatText(p Payload, zones Zones, limit int) string {
	return truncate("⚠️ "+Sentence(p, zones), limit)
}

// DiscordBody is a plain content message; mentions are disabled so an app name
// can never ping anyone.
func DiscordBody(p Payload, zones Zones) []byte {
	data, _ := json.Marshal(struct {
		Content         string `json:"content"`
		AllowedMentions struct {
			Parse []string `json:"parse"`
		} `json:"allowed_mentions"`
	}{Content: ChatText(p, zones, DiscordMaxRunes), AllowedMentions: struct {
		Parse []string `json:"parse"`
	}{Parse: []string{}}})
	return data
}

// SlackBody is the minimal incoming webhook payload, without Block Kit.
func SlackBody(p Payload, zones Zones) []byte {
	data, _ := json.Marshal(struct {
		Text string `json:"text"`
	}{Text: ChatText(p, zones, SlackMaxRunes)})
	return data
}

// NtfyBody publishes through JSON with the topic as a field, so the topic is
// never concatenated into a URL path. No email, call, attachment, click,
// action or delay is ever requested.
func NtfyBody(p Payload, zones Zones, topic string) []byte {
	priority := 4
	if Severity(p) == "critical" {
		priority = 5
	}
	data, _ := json.Marshal(struct {
		Topic    string   `json:"topic"`
		Title    string   `json:"title"`
		Message  string   `json:"message"`
		Priority int      `json:"priority"`
		Tags     []string `json:"tags"`
	}{Topic: topic, Title: ntfyTitle, Message: truncate("Comporta: "+Sentence(p, zones), NtfyMaxRunes), Priority: priority, Tags: []string{"warning"}})
	return data
}
