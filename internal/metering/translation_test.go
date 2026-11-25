package metering

import (
	"context"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/events"
	"github.com/JoaoVictorVM/comporta/internal/proxy"
	"github.com/JoaoVictorVM/comporta/internal/storage"
	"github.com/JoaoVictorVM/comporta/internal/translation"
)

func skipOf(position int) proxy.TranslationSkip {
	return proxy.TranslationSkip{At: time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC), Position: position, Model: "fixture",
		Source: translation.OpenAIChat, Target: translation.AnthropicMessages, Feature: translation.FeatureToolChoice}
}

func TestTranslationSkipIsDurable(t *testing.T) {
	s, store, _, e := setup(t)
	for range 2 {
		if err := s.RecordTranslationSkip(context.Background(), e, skipOf(2)); err != nil {
			t.Fatal(err)
		}
	}
	skips := eventsOf(t, store, events.TranslationSkipped)
	if len(skips) != 1 {
		t.Fatalf("a repeated skip must be one event, got %d", len(skips))
	}
	p := skips[0].Payload
	if p.ChainPosition != 2 || p.Model != "fixture" || p.Feature != "tool_choice" || p.Reason != events.ReasonUntranslatable ||
		p.SourceFormat != "openai_chat" || p.TargetFormat != "anthropic_messages" || skips[0].UserHash != "" || p.AttemptIndex != 0 {
		t.Fatalf("payload %+v", skips[0])
	}
	if err := s.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	page, err := store.Records(context.Background(), storage.RecordFilter{AppName: "bot", Limit: 10})
	if err != nil || len(page.Records) != 0 {
		t.Fatalf("a skip must not create a request record: %+v %v", page.Records, err)
	}
}

func TestTranslationSkipFailureStopsOperation(t *testing.T) {
	s, store, _, e := setup(t)
	store.Close()
	if err := s.RecordTranslationSkip(context.Background(), e, skipOf(1)); err == nil {
		t.Fatal("an undurable skip must be reported, so the next candidate is not sent")
	}
}

func TestTranslationUsesRenderedBodyEstimate(t *testing.T) {
	s, _, snapshot, e := setup(t)
	small, large := candidates(snapshot, e, false)[0], candidates(snapshot, e, false)[0]
	large.BodySizeEffective = e.BodySizeEffective * 4
	smallQuote, rejection := s.price(snapshot, e, small)
	if rejection != nil {
		t.Fatal(rejection.Message)
	}
	largeQuote, rejection := s.price(snapshot, e, large)
	if rejection != nil {
		t.Fatal(rejection.Message)
	}
	if largeQuote.tokens <= smallQuote.tokens || largeQuote.reserved <= smallQuote.reserved {
		t.Fatalf("the quote must follow the rendered body: %d/%d tokens, %d/%d reserved", smallQuote.tokens, largeQuote.tokens, smallQuote.reserved, largeQuote.reserved)
	}
}
