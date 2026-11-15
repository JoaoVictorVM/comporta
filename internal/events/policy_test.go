package events

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func warnedEvent() Event {
	return NewPolicyAdmission(Warned, at, strings.Repeat("2", 32), 1, "bot", "apps.bot.budget.usd_per_day", "app", "spent", "modelo-principal", "modelo-principal")
}

func TestPolicyPayloadAllowlist(t *testing.T) {
	if err := warnedEvent().Validate(); err != nil {
		t.Fatal(err)
	}
	downgraded := NewPolicyAdmission(Downgraded, at, strings.Repeat("2", 32), 1, "bot", "apps.bot.per_user.budget.usd_per_day", "user", "reservation", "modelo-principal", "modelo-economico")
	if err := downgraded.Validate(); err != nil {
		t.Fatal(err)
	}
	if downgraded.ID == warnedEvent().ID || downgraded.ID != PolicyEventID(downgraded.RequestID, 1, Downgraded) {
		t.Fatal("policy event IDs must be deterministic per request, attempt and type")
	}
	for name, mutate := range map[string]func(*Event){
		"free text model":  func(e *Event) { e.Payload.ModelUsed = "a model with spaces" },
		"long model":       func(e *Event) { e.Payload.ModelUsed = strings.Repeat("m", 257) },
		"missing models":   func(e *Event) { e.Payload.ModelRequested, e.Payload.ModelUsed = "", "" },
		"missing limit":    func(e *Event) { e.Payload.LimitName = "" },
		"bad limit":        func(e *Event) { e.Payload.LimitName = "prompt text" },
		"bad scope":        func(e *Event) { e.Payload.Scope = "global" },
		"user subject":     func(e *Event) { anonymous := false; e.UserHash, e.Anonymous = strings.Repeat("3", 32), &anonymous },
		"wrong outcome":    func(e *Event) { e.Payload.Outcome = "ok" },
		"error text":       func(e *Event) { e.Payload.ErrorCode = "raw provider message" },
		"no request":       func(e *Event) { e.RequestID = "" },
		"models elsewhere": func(e *Event) { e.Type = Blocked },
	} {
		e := warnedEvent()
		mutate(&e)
		if e.Validate() == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if !ValidType(Warned) {
		t.Fatal("warned must be a readable event type")
	}
}

func TestWarnedEventRoundTrip(t *testing.T) {
	e := warnedEvent()
	data, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"event_id":"` + "%s" + `","type":"warned","occurred_at":"2026-11-11T15:00:00Z","request_id":"22222222222222222222222222222222","app":"bot","payload":{"outcome":"warned","attempt_index":1,"reason":"spent","limit_name":"apps.bot.budget.usd_per_day","scope":"app","model_requested":"modelo-principal","model_used":"modelo-principal"}}`
	if string(data) != strings.Replace(want, "%s", e.ID, 1) {
		t.Fatalf("serialized event: %s", data)
	}
	var back Event
	if err = json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, e) || back.Validate() != nil {
		t.Fatalf("round trip changed the event: %+v", back)
	}
}
