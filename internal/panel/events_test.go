package panel

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/events"
)

func TestEventFormatterAllowlist(t *testing.T) {
	anonymous := false
	cut := events.NewLoopCut(strings.Repeat("a", 32), time.Now(), strings.Repeat("b", 32), "bot", strings.Repeat("c", 32), anonymous, events.ReasonSimilarRequests, 10, 30*time.Second, time.Minute)
	row, ok := formatEvent(cut)
	if !ok || row.Label != "Loop cut" || row.User != "u_cccccccc" {
		t.Fatalf("loop cut: %+v", row)
	}
	data, _ := json.Marshal(row)
	if strings.Contains(string(data), strings.Repeat("c", 32)) {
		t.Fatal("the full pseudonym reached the row")
	}
	completed := events.Completion(strings.Repeat("b", 32), 1, time.Now(), "bot", "ok", 10, false)
	if _, ok := formatEvent(completed); ok {
		t.Fatal("a type outside the screen was formatted")
	}
	if _, ok := eventLabel(events.SensitiveDataDetected); ok {
		t.Fatal("sensitive detections are listed")
	}
}

func TestConfigEventStaysMasked(t *testing.T) {
	old, next := "[REDACTED]", "[REDACTED]"
	e := events.New(events.ConfigChanged, time.Now())
	e.Payload.Generation, e.Payload.Result = 3, config.ResultApplied
	e.Payload.Changes = []config.Change{{Path: "providers.anthropic.api_key", Old: &old, New: &next, SecretChanged: true}, {Path: "apps.bot.budget.usd_per_day", Old: ptr("1"), New: ptr("2")}}
	row, ok := formatEvent(e)
	if !ok {
		t.Fatal("config event not formatted")
	}
	text := strings.Join(row.Details, "\n")
	if !strings.Contains(text, "providers.anthropic.api_key: secret changed") || !strings.Contains(text, "apps.bot.budget.usd_per_day: 1 -> 2") || !strings.Contains(text, "generation: 3") {
		t.Fatalf("details: %s", text)
	}
}

func ptr(s string) *string { return &s }

func TestEventsNewestFirst(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	base := time.Now().Add(-time.Hour)
	for n := range 60 {
		e := events.New(events.ConfigChanged, base.Add(time.Duration(n)*time.Second))
		e.Payload.Generation, e.Payload.Result = int64(n+1), config.ResultApplied
		if err := env.store.Publish(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	paused := true
	p := events.New(events.Paused, base.Add(2*time.Hour))
	p.Payload.Paused, p.Payload.Scope = &paused, "global"
	if err := env.store.Publish(ctx, p); err != nil {
		t.Fatal(err)
	}
	list, err := env.service.Events(ctx, EventQuery{})
	if err != nil || len(list.Rows) != 50 || list.Rows[0].Type != "paused" || list.Next == "" {
		t.Fatalf("first page: %v %+v", err, list.Rows[:1])
	}
	next, err := env.service.Events(ctx, EventQuery{Cursor: list.Next})
	if err != nil || len(next.Rows) != 11 || next.Next != "" {
		t.Fatalf("second page: %v %d", err, len(next.Rows))
	}
	only, _ := env.service.Events(ctx, EventQuery{Type: "paused"})
	if len(only.Rows) != 1 || !strings.Contains(strings.Join(only.Rows[0].Details, " "), "state: paused") {
		t.Fatalf("type filter: %+v", only.Rows)
	}
	if r, _ := env.service.Events(ctx, EventQuery{Type: "request_completed"}); !r.Invalid {
		t.Fatal("a type outside the screen was accepted")
	}
	if r, _ := env.service.Events(ctx, EventQuery{Type: "paused", Cursor: list.Next}); !r.Invalid {
		t.Fatal("a cursor crossed filters")
	}
}
