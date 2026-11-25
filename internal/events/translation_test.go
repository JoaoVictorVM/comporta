package events

import (
	"strings"
	"testing"
	"time"
)

func skip() Event {
	return NewTranslationSkip(time.Now(), strings.Repeat("a", 32), "bot", 2, "gpt-4.1", "anthropic_messages", "openai_chat", "provider_beta")
}

func TestTranslationSkippedValidation(t *testing.T) {
	if err := skip().ValidateTranslationSkip(); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*Event){
		"no request":    func(e *Event) { e.RequestID = "" },
		"no app":        func(e *Event) { e.App = "" },
		"open feature":  func(e *Event) { e.Payload.Feature = "something_else" },
		"open format":   func(e *Event) { e.Payload.TargetFormat = "gemini" },
		"same format":   func(e *Event) { e.Payload.TargetFormat = e.Payload.SourceFormat },
		"attempt":       func(e *Event) { e.Payload.AttemptIndex = 2 },
		"subject":       func(e *Event) { e.UserHash = strings.Repeat("b", 32); flag := false; e.Anonymous = &flag },
		"reason":        func(e *Event) { e.Payload.Reason = "spent" },
		"position":      func(e *Event) { e.Payload.ChainPosition = 100 },
		"unbound model": func(e *Event) { e.Payload.Model = "bad model" },
	} {
		e := skip()
		change(&e)
		if e.ValidateTranslationSkip() == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	other := NewFallback(time.Now(), strings.Repeat("a", 32), "bot", 2, "a", "b", "429")
	other.Payload.Feature = "provider_beta"
	if other.Validate() == nil {
		t.Fatal("skip fields must stay on skip events")
	}
}

func TestTranslationSkippedIDStable(t *testing.T) {
	a, b := skip(), skip()
	if a.ID != b.ID || !eventID.MatchString(a.ID) {
		t.Fatalf("ids %s %s", a.ID, b.ID)
	}
	if TranslationSkipID(a.RequestID, 3, "gpt-4.1", "provider_beta") == a.ID || TranslationSkipID(a.RequestID, 2, "gpt-4.1", "tool_choice") == a.ID {
		t.Fatal("position and feature must change the id")
	}
}

func TestTranslationPayloadRejectsContent(t *testing.T) {
	e := skip()
	e.Payload.Model = "Qual é o clima em Recife? " + strings.Repeat("x", 300)
	if e.Validate() == nil {
		t.Fatal("free text accepted as a model name")
	}
	e = skip()
	e.Payload.Feature = "tool_choice: any of get_weather"
	if e.Validate() == nil {
		t.Fatal("free text accepted as a feature")
	}
}
