package events

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/JoaoVictorVM/comporta/internal/sensitivedata"
)

func sensitiveEvent() Event {
	return NewSensitiveDetection(at, strings.Repeat("2", 32), "support", strings.Repeat("3", 32), false, sensitivedata.Summary{
		{DataType: sensitivedata.Email, Count: 1, Action: sensitivedata.ModeRedact},
	})
}

func TestSensitivePayloadValidation(t *testing.T) {
	e := sensitiveEvent()
	if err := e.ValidateSensitive(); err != nil {
		t.Fatal(err)
	}
	if e.ID != SensitiveScanID(e.RequestID) || e.ID == SensitiveScanID(strings.Repeat("4", 32)) {
		t.Fatal("event ID must derive from the request")
	}
	data, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"event_id":"` + e.ID + `","type":"sensitive_data_detected","occurred_at":"2026-11-11T15:00:00Z","request_id":"` + e.RequestID +
		`","app":"support","user_hash":"` + e.UserHash + `","anonymous":false,"payload":{"detections":[{"data_type":"email","count":1,"action":"redact"}]}}`
	if string(data) != want {
		t.Fatalf("json = %s", data)
	}
	for name, mutate := range map[string]func(*Event){
		"count zero":         func(e *Event) { e.Payload.Detections[0].Count = 0 },
		"action off":         func(e *Event) { e.Payload.Detections[0].Action = sensitivedata.ModeOff },
		"extra payload":      func(e *Event) { e.Payload.Count = 3 },
		"error code":         func(e *Event) { e.Payload.ErrorCode = "loop_detected" },
		"loop metadata":      func(e *Event) { e.Payload.WindowMS = 1 },
		"empty detections":   func(e *Event) { e.Payload.Detections = sensitivedata.Summary{} },
		"kinds outside":      func(e *Event) { e.Payload.Detections[0].SecretKinds = []string{"jwt"} },
		"detections on loop": func(e *Event) { e.Type = LoopCut },
	} {
		event := sensitiveEvent()
		mutate(&event)
		if event.ValidateSensitive() == nil {
			t.Errorf("%s accepted", name)
		}
	}
	other := New(RequestCompleted, at)
	other.Payload.Detections = e.Payload.Detections
	if other.Validate() == nil {
		t.Fatal("detections accepted on another type")
	}
}

func TestLegacySensitiveEventRead(t *testing.T) {
	legacy := New(SensitiveDataDetected, at)
	if err := legacy.Validate(); err != nil {
		t.Fatalf("legacy event no longer valid: %v", err)
	}
	if legacy.ValidateSensitive() == nil {
		t.Fatal("the new contract must require the payload")
	}
	var read Event
	if err := json.Unmarshal([]byte(`{"event_id":"`+legacy.ID+`","type":"sensitive_data_detected","occurred_at":"2026-01-01T00:00:00Z","payload":{}}`), &read); err != nil || read.Validate() != nil {
		t.Fatalf("legacy read: %v", err)
	}
}

func TestSensitiveEventClone(t *testing.T) {
	e := NewSensitiveDetection(at, strings.Repeat("2", 32), "support", strings.Repeat("3", 32), true, sensitivedata.Summary{
		{DataType: sensitivedata.Secret, Count: 1, Action: sensitivedata.ModeBlock, SecretKinds: []string{sensitivedata.KindJWT}},
	})
	clone := e.Clone()
	clone.Payload.Detections[0].Count = 9
	clone.Payload.Detections[0].SecretKinds[0] = "x"
	*clone.Anonymous = false
	if e.Payload.Detections[0].Count != 1 || e.Payload.Detections[0].SecretKinds[0] != sensitivedata.KindJWT || !*e.Anonymous {
		t.Fatal("clone shares memory with the original")
	}
}

func TestSensitiveSubjectValidation(t *testing.T) {
	for name, mutate := range map[string]func(*Event){
		"hash without flag": func(e *Event) { e.Anonymous = nil },
		"flag without hash": func(e *Event) { e.UserHash = "" },
		"bad hash":          func(e *Event) { e.UserHash = "x" },
		"no app":            func(e *Event) { e.App = "" },
		"no request":        func(e *Event) { e.RequestID = "" },
	} {
		event := sensitiveEvent()
		mutate(&event)
		if event.ValidateSensitive() == nil {
			t.Errorf("%s accepted", name)
		}
	}
	paused := New(Paused, at)
	flag := true
	paused.Payload.Paused, paused.Payload.Scope, paused.App = &flag, "app", "support"
	paused.UserHash, paused.Anonymous = strings.Repeat("3", 32), &flag
	if paused.Validate() == nil {
		t.Fatal("a subject was accepted on a pause event")
	}
}
