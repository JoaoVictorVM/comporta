package events

import (
	"strings"
	"testing"
	"time"
)

var at = time.Date(2026, 11, 11, 15, 0, 0, 0, time.UTC)

func loopEvent() Event {
	return NewLoopCut(strings.Repeat("1", 32), at, strings.Repeat("2", 32), "agent-app", strings.Repeat("3", 32), false, ReasonSimilarRequests, 11, 30*time.Second, 120*time.Second)
}

func TestLoopPayloadValidation(t *testing.T) {
	e := loopEvent()
	if err := e.ValidateLoopCut(); err != nil {
		t.Fatal(err)
	}
	if e.Payload.WindowMS != 30000 || e.Payload.CooldownMS != 120000 || e.Payload.ErrorCode != "loop_detected" || e.Payload.Outcome != "loop_cut" {
		t.Fatalf("payload: %+v", e.Payload)
	}
	sub := NewLoopCut(e.ID, at, e.RequestID, e.App, e.UserHash, false, ReasonRequestRate, 61, 1500*time.Microsecond, time.Nanosecond)
	if sub.Payload.WindowMS != 2 || sub.Payload.CooldownMS != 1 {
		t.Fatalf("sub-millisecond durations must round up: %+v", sub.Payload)
	}
	for name, mutate := range map[string]func(*Event){
		"reason":   func(e *Event) { e.Payload.Reason = "spent" },
		"unknown":  func(e *Event) { e.Payload.Reason = "other" },
		"window":   func(e *Event) { e.Payload.WindowMS = -1 },
		"no count": func(e *Event) { e.Payload.Count = 0 },
		"no wind":  func(e *Event) { e.Payload.WindowMS = 0 },
	} {
		bad := loopEvent()
		mutate(&bad)
		if bad.ValidateLoopCut() == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestLoopSubjectPairing(t *testing.T) {
	missingFlag := loopEvent()
	missingFlag.Anonymous = nil
	missingHash := loopEvent()
	missingHash.UserHash = ""
	badHash := loopEvent()
	badHash.UserHash = strings.Repeat("Z", 32)
	noApp := loopEvent()
	noApp.App = ""
	for name, e := range map[string]Event{"flag": missingFlag, "bad hash": badHash, "no app": noApp} {
		if e.Validate() == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if missingHash.ValidateLoopCut() == nil {
		t.Error("a new loop cut needs its subject")
	}
}

func TestLoopFieldsRejectedOnOtherEvents(t *testing.T) {
	yes := true
	for name, mutate := range map[string]func(*Event){
		"hash":     func(e *Event) { e.UserHash = strings.Repeat("3", 32) },
		"flag":     func(e *Event) { e.Anonymous = &yes },
		"window":   func(e *Event) { e.Payload.WindowMS = 1 },
		"cooldown": func(e *Event) { e.Payload.CooldownMS = 1 },
		"reason":   func(e *Event) { e.Payload.Reason = ReasonRequestRate },
	} {
		e := Event{ID: strings.Repeat("1", 32), Type: Blocked, At: at, App: "agent-app"}
		if err := e.Validate(); err != nil {
			t.Fatal(err)
		}
		mutate(&e)
		if e.Validate() == nil {
			t.Errorf("%s accepted on a blocked event", name)
		}
	}
}

func TestLegacyLoopEventReadable(t *testing.T) {
	legacy := Event{ID: strings.Repeat("1", 32), Type: LoopCut, At: at, App: "agent-app", Payload: Payload{Outcome: "loop_cut", Count: 11}}
	if err := legacy.Validate(); err != nil {
		t.Fatalf("older loop cut unreadable: %v", err)
	}
}

func TestCloneCopiesAnonymous(t *testing.T) {
	e := loopEvent()
	c := e.Clone()
	*c.Anonymous = true
	if *e.Anonymous {
		t.Fatal("clone shares the anonymous flag")
	}
}
