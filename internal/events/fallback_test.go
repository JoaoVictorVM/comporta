package events

import (
	"encoding/json"
	"testing"
	"time"
)

const fallbackRequestID = "22222222222222222222222222222222"

func TestFallbackEventValidation(t *testing.T) {
	at := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	for cause, class := range map[string]string{"429": "rate_limit", "529": "overload", "500": "upstream_5xx", "503": "upstream_5xx", "connection": "connection", "timeout": "timeout"} {
		e := NewFallback(at, fallbackRequestID, "bot-discord", 2, "claude-sonnet-4-5", "claude-haiku-4-5", cause)
		if err := e.ValidateFallback(); err != nil || e.Payload.ErrorClass != class {
			t.Fatalf("cause %s: %v %+v", cause, err, e.Payload)
		}
	}
	valid := NewFallback(at, fallbackRequestID, "bot-discord", 2, "claude-sonnet-4-5", "claude-haiku-4-5", "529")
	for name, change := range map[string]func(*Event){
		"cause outside the set": func(e *Event) { e.Payload.Cause = "400" },
		"class does not match":  func(e *Event) { e.Payload.ErrorClass = "timeout" },
		"missing origin":        func(e *Event) { e.Payload.FromModel = "" },
		"model with spaces":     func(e *Event) { e.Payload.ToModel = "model b" },
		"first attempt":         func(e *Event) { e.Payload.AttemptIndex = 1; e.ID = FallbackEventID(e.RequestID, 1) },
		"beyond five attempts":  func(e *Event) { e.Payload.AttemptIndex = 6; e.ID = FallbackEventID(e.RequestID, 6) },
		"random identifier":     func(e *Event) { e.ID = New(Fallback, at).ID },
		"user subject":          func(e *Event) { anonymous := false; e.UserHash, e.Anonymous = fallbackRequestID, &anonymous },
		"without request":       func(e *Event) { e.RequestID = "" },
		"with a cost":           func(e *Event) { e.Payload.Cost = 1 },
	} {
		e := valid
		change(&e)
		if e.ValidateFallback() == nil {
			t.Errorf("%s accepted", name)
		}
	}
	// Transition fields never appear on another type.
	other := Completion(fallbackRequestID, 2, at, "bot-discord", "fallback", 0, false)
	other.Payload.FromModel = "claude-sonnet-4-5"
	if other.Validate() == nil {
		t.Fatal("transition fields on a completion event")
	}
}

func TestLegacyFallbackEventReadable(t *testing.T) {
	// A fallback event written before the transition fields existed stays
	// readable, without inventing a transition.
	legacy := New(Fallback, time.Now())
	legacy.RequestID, legacy.App = fallbackRequestID, "bot"
	if err := legacy.Validate(); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(legacy.Payload)
	if err != nil || string(data) != "{}" {
		t.Fatalf("legacy payload %s %v", data, err)
	}
	if legacy.ValidateFallback() == nil {
		t.Fatal("a new producer may not omit the transition")
	}
}

func TestFallbackAdmissionEventExactlyOnce(t *testing.T) {
	at := time.Now()
	a := NewFallback(at, fallbackRequestID, "bot", 3, "model-b", "model-c", "timeout")
	b := NewFallback(at, fallbackRequestID, "bot", 3, "model-b", "model-c", "timeout")
	if a.ID != b.ID || a.ID == FallbackEventID(fallbackRequestID, 2) || a.ID == PolicyEventID(fallbackRequestID, 3, Downgraded) {
		t.Fatal("the transition identifier is not derived from the request and attempt alone")
	}
	data, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Event
	if err = json.Unmarshal(data, &decoded); err != nil || decoded.ValidateFallback() != nil || decoded.Payload.Cause != "timeout" {
		t.Fatalf("round trip %s %v", data, err)
	}
}
