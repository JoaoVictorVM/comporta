package events

import (
	"context"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/config"
)

func TestLiveSubscriberCannotBlockAccounting(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bus := &Bus{}
	slow := bus.Subscribe(ctx, RequestCompleted)
	e := Completion("0123456789abcdef0123456789abcdef", 1, time.Now(), "bot", "ok", 49, false)
	for range 1000 {
		bus.Notify(e)
	}
	if !slow.Resync.Load() || slow.Lost.Load() != 744 {
		t.Fatal("overflow did not signal resync")
	}
	for range 256 {
		<-slow.Events
	}
	bus.Notify(e)
	select {
	case <-slow.Events:
	case <-time.After(time.Second):
		t.Fatal("subscriber stopped receiving")
	}
}

func TestTypedPayloadRejectsContent(t *testing.T) {
	e := Completion("0123456789abcdef0123456789abcdef", 1, time.Now(), "bot", "raw provider content", 49, false)
	if err := e.Validate(); err == nil {
		t.Fatal("free-text outcome accepted")
	}
	e.Payload.Outcome = "ok"
	e.Payload.Changes = []config.Change{{Path: "body"}}
	if err := e.Validate(); err == nil {
		t.Fatal("request event accepted config content")
	}
}

func TestSubscriberPayloadsAreIndependent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bus := &Bus{}
	a, b := bus.Subscribe(ctx), bus.Subscribe(ctx)
	value := "[REDACTED]"
	e := New(ConfigChanged, time.Now())
	e.Payload = Payload{Result: config.ResultApplied, Changes: []config.Change{{Path: "providers.test.api_key", New: &value}}}
	bus.Notify(e)
	first := <-a.Events
	*first.Payload.Changes[0].New = "changed"
	second := <-b.Events
	if *second.Payload.Changes[0].New != "[REDACTED]" {
		t.Fatal("subscriber changed another payload")
	}
}
