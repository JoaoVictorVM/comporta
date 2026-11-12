package metering

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/events"
	"github.com/JoaoVictorVM/comporta/internal/loopguard"
	"github.com/JoaoVictorVM/comporta/internal/storage"
	"github.com/JoaoVictorVM/comporta/internal/storage/sqlite"
)

func cutDecision() loopguard.Decision {
	return loopguard.Decision{First: true, Remaining: 120 * time.Second, Episode: loopguard.Episode{
		EventID: "11111111111111111111111111111111", At: time.Date(2026, 11, 11, 15, 0, 0, 123456789, time.UTC),
		RequestID: "0123456789abcdef0123456789abcdef", Reason: loopguard.ReasonSimilar, Count: 11, Limit: 10,
		Window: 30 * time.Second, Cooldown: 120 * time.Second,
	}}
}

func TestLoopCutHasNoReservation(t *testing.T) {
	service, store, snapshot, envelope := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sub := store.Bus.Subscribe(ctx)
	if err := service.RecordLoopCut(ctx, snapshot, envelope, cutDecision()); err != nil {
		t.Fatal(err)
	}
	pending, err := store.Pending(ctx)
	if err != nil || len(pending) != 0 {
		t.Fatalf("a cut entered the journal: %v %v", pending, err)
	}
	select {
	case e := <-sub.Events:
		if e.Type != events.LoopCut || e.UserHash != envelope.Identity.Hash().Hex() || e.Payload.Count != 11 || e.At.Nanosecond()%1000 != 0 {
			t.Fatalf("event: %+v", e)
		}
	case <-time.After(time.Second):
		t.Fatal("no loop event")
	}
}

func TestLoopCutZeroUsage(t *testing.T) {
	service, store, snapshot, envelope := setup(t)
	ctx := context.Background()
	if err := service.RecordLoopCut(ctx, snapshot, envelope, cutDecision()); err != nil {
		t.Fatal(err)
	}
	page, err := store.Records(ctx, storage.RecordFilter{AppName: "bot", Limit: 10})
	if err != nil || len(page.Records) != 1 {
		t.Fatalf("records: %v", err)
	}
	r := page.Records[0]
	if r.Outcome != "loop_cut" || r.AttemptIndex != 0 || r.RequestCount != 0 || r.Cost != 0 || r.ReservedCost != 0 || r.Input != nil || r.UsageMissing ||
		r.UpstreamStatus != nil || r.DownstreamStatus == nil || *r.DownstreamStatus != 429 || r.ErrorCode != "loop_detected" || r.Provider != "" || r.ModelUsed != "" {
		t.Fatalf("cut record: %+v", r)
	}
	daily, err := store.Daily(ctx, storage.Filter{AppName: "bot", Since: "2000-01-01", Until: "2100-01-01"})
	if err != nil || len(daily) != 0 {
		t.Fatalf("a cut was counted as usage: %+v", daily)
	}
	// A retry in the same episode stores its own record and reuses the event.
	envelope.RequestID = "fedcba9876543210fedcba9876543210"
	retry := cutDecision()
	retry.First = false
	if err := service.RecordLoopCut(ctx, snapshot, envelope, retry); err != nil {
		t.Fatal(err)
	}
	page, _ = store.Records(ctx, storage.RecordFilter{AppName: "bot", Limit: 10})
	events, _ := store.ReadEvents(ctx, events.Filter{Types: []events.Type{events.LoopCut}})
	if len(page.Records) != 2 || len(events.Events) != 1 {
		t.Fatalf("records=%d events=%d", len(page.Records), len(events.Events))
	}
}

type failingCuts struct{ *sqlite.Store }

func (failingCuts) CommitLocalCut(context.Context, storage.LocalCut) error {
	return storage.ErrUnavailable
}

type noCuts struct {
	storage.AdmissionStore
	storage.CompletionStore
	storage.MaintenanceStore
}

func TestLoopStorageFailure(t *testing.T) {
	service, store, snapshot, envelope := setup(t)
	failing := New(failingCuts{store}, service.catalog, service.logger, service.retention)
	if err := failing.RecordLoopCut(context.Background(), snapshot, envelope, cutDecision()); !errors.Is(err, storage.ErrUnavailable) {
		t.Fatalf("failure hidden: %v", err)
	}
	missing := New(noCuts{store, store, store}, service.catalog, service.logger, service.retention)
	if err := missing.RecordLoopCut(context.Background(), snapshot, envelope, cutDecision()); !errors.Is(err, storage.ErrUnavailable) {
		t.Fatalf("store without cuts: %v", err)
	}
}
