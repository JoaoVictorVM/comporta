package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/events"
	"github.com/JoaoVictorVM/comporta/internal/storage"
)

func TestPublishAfterCommitOnly(t *testing.T) {
	s := testStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sub := s.Bus.Subscribe(ctx)
	e := events.New(events.Paused, time.Now())
	e.App = "bot"
	e.Payload.Paused = true
	if err := s.Publish(ctx, e); err != nil {
		t.Fatal(err)
	}
	page, err := s.ReadEvents(ctx, events.Filter{App: "bot", Types: []events.Type{events.Paused}, Limit: 1})
	if err != nil || len(page.Events) != 1 || page.Events[0].ID != e.ID {
		t.Fatal("durable event query missing")
	}
	select {
	case got := <-sub.Events:
		if got.ID != e.ID {
			t.Fatal("event changed")
		}
	case <-time.After(time.Second):
		t.Fatal("no notification")
	}
	if err := s.Publish(ctx, e); err != nil {
		t.Fatal(err)
	}
	select {
	case <-sub.Events:
		t.Fatal("duplicate live event")
	default:
	}
	e.Payload.Paused = false
	if err := s.Publish(ctx, e); err != storage.ErrConflict {
		t.Fatal("incompatible retry accepted")
	}
	e = events.New(events.ConfigChanged, time.Now())
	e.Payload.Result = config.ResultApplied
	e.Payload.Generation = 2
	if err := s.Publish(ctx, e); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT count(*) FROM config_changes`).Scan(&count); err != nil || count != 1 {
		t.Fatal("configuration diff missing")
	}
}
