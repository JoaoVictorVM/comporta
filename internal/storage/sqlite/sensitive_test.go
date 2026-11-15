package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/events"
	"github.com/JoaoVictorVM/comporta/internal/sensitivedata"
	"github.com/JoaoVictorVM/comporta/internal/storage"
)

const sensitiveHash = "44444444444444444444444444444444"

func sensitiveSummary() sensitivedata.Summary {
	return sensitivedata.Summary{
		{DataType: sensitivedata.Email, Count: 2, Action: sensitivedata.ModeRedact},
		{DataType: sensitivedata.Secret, Count: 1, Action: sensitivedata.ModeBlock, SecretKinds: []string{sensitivedata.KindAnthropicAPIKey}},
	}
}

func sensitiveCut(n int, blocked bool) storage.SensitiveCut {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	status, code, class := 400, "sensitive_data_blocked", "invalid_request"
	if !blocked {
		status, code, class = 503, "sensitive_data_scan_failed", "internal"
	}
	id := fmt.Sprintf("%032x", n)
	r := storage.Record{RequestID: id, AttemptIndex: 0, StartedAt: now, FinishedAt: now, AppName: "support", UserHash: sensitiveHash,
		Operation: "generation", ModelRequested: "m", Outcome: "blocked", ErrorClass: class, ErrorCode: code,
		DownstreamStatus: &status, Day: "2026-11-15", Timezone: "UTC"}
	cut := storage.SensitiveCut{Record: r}
	if blocked {
		cut.Record.Detections = sensitiveSummary()
		e := events.NewSensitiveDetection(now, id, "support", sensitiveHash, false, sensitiveSummary())
		cut.Event = &e
	}
	return cut
}

func TestSensitiveCutAtomic(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.CommitSensitiveCut(ctx, sensitiveCut(1, true)); err != nil {
		t.Fatal(err)
	}
	if count(t, s, `SELECT count(*) FROM requests WHERE outcome='blocked' AND attempt_index=0 AND cost_microusd=0 AND reserved_cost_microusd=0 AND request_count=0`) != 1 {
		t.Fatal("cut record missing")
	}
	if count(t, s, `SELECT count(*) FROM events WHERE type='sensitive_data_detected' AND user_hash=? AND anonymous=0`, sensitiveHash) != 1 {
		t.Fatal("event without its subject columns")
	}
	if count(t, s, `SELECT count(*) FROM metering_journal`)+count(t, s, `SELECT count(*) FROM usage_daily`)+count(t, s, `SELECT count(*) FROM limit_reservations`) != 0 {
		t.Fatal("a cut must not touch journal, aggregates or reservations")
	}
	if err := s.CommitSensitiveCut(ctx, sensitiveCut(2, false)); err != nil {
		t.Fatal(err)
	}
	if count(t, s, `SELECT count(*) FROM events`) != 1 || count(t, s, `SELECT count(*) FROM requests`) != 2 {
		t.Fatal("a failed scan must store its record and no event")
	}

	invalid := []func(*storage.SensitiveCut){
		func(c *storage.SensitiveCut) { c.Event = nil },
		func(c *storage.SensitiveCut) { c.Record.ErrorCode = "loop_detected" },
		func(c *storage.SensitiveCut) { c.Record.AttemptIndex = 1 },
		func(c *storage.SensitiveCut) { c.Event.UserHash = loopHash },
		func(c *storage.SensitiveCut) {
			c.Record.Detections = sensitivedata.Summary{{DataType: sensitivedata.CPF, Count: 0, Action: sensitivedata.ModeDetect}}
		},
		func(c *storage.SensitiveCut) { c.Event.Payload.Detections = nil },
	}
	for i, change := range invalid {
		cut := sensitiveCut(10+i, true)
		change(&cut)
		if err := s.CommitSensitiveCut(ctx, cut); !errors.Is(err, storage.ErrInvalid) {
			t.Errorf("case %d: %v", i, err)
		}
	}
}

func TestSensitiveCutIdempotent(t *testing.T) {
	s := testStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sub := s.Bus.Subscribe(ctx)
	for range 3 {
		if err := s.CommitSensitiveCut(ctx, sensitiveCut(1, true)); err != nil {
			t.Fatal(err)
		}
	}
	if count(t, s, `SELECT count(*) FROM requests`) != 1 || count(t, s, `SELECT count(*) FROM events`) != 1 {
		t.Fatal("a repeated commit duplicated rows")
	}
	select {
	case <-sub.Events:
	case <-time.After(time.Second):
		t.Fatal("no notification")
	}
	select {
	case e := <-sub.Events:
		t.Fatalf("notified twice: %+v", e)
	case <-time.After(50 * time.Millisecond):
	}
	conflicting := sensitiveCut(1, true)
	conflicting.Record.ModelRequested = "other"
	if err := s.CommitSensitiveCut(ctx, conflicting); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("conflict: %v", err)
	}
}

func TestSensitiveCutRollback(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.CommitSensitiveCut(ctx, sensitiveCut(1, true)); err != nil {
		t.Fatal(err)
	}
	// Same event ID, different payload: the new record must not stay without it.
	other := sensitiveCut(2, true)
	clash := events.NewSensitiveDetection(other.Record.StartedAt, other.Record.RequestID, "support", sensitiveHash, false, sensitivedata.Summary{{DataType: sensitivedata.CPF, Count: 1, Action: sensitivedata.ModeBlock}})
	clash.ID = events.SensitiveScanID(fmt.Sprintf("%032x", 1))
	other.Event = &clash
	other.Record.Detections = clash.Payload.Detections
	if err := s.CommitSensitiveCut(ctx, other); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("clash: %v", err)
	}
	if count(t, s, `SELECT count(*) FROM requests`) != 1 {
		t.Fatal("record committed without its event")
	}
}

func TestSensitiveEventReadSubject(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	at := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	event := events.NewSensitiveDetection(at, fmt.Sprintf("%032x", 7), "support", sensitiveHash, true, sensitiveSummary())
	if err := s.Publish(ctx, event); err != nil {
		t.Fatal(err)
	}
	legacy := events.New(events.SensitiveDataDetected, at.Add(time.Second))
	if err := s.Publish(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	page, err := s.ReadEvents(ctx, events.Filter{Types: []events.Type{events.SensitiveDataDetected}})
	if err != nil || len(page.Events) != 2 {
		t.Fatalf("events: %v %+v", err, page)
	}
	read := page.Events[0]
	if read.UserHash != sensitiveHash || read.Anonymous == nil || !*read.Anonymous || read.ValidateSensitive() != nil {
		t.Fatalf("read = %+v", read)
	}
	if page.Events[1].Payload.Detections != nil || page.Events[1].UserHash != "" {
		t.Fatalf("legacy = %+v", page.Events[1])
	}
}

func TestSensitiveSummaryRoundTrip(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.CommitSensitiveCut(ctx, sensitiveCut(1, true)); err != nil {
		t.Fatal(err)
	}
	page, err := s.Records(ctx, storage.RecordFilter{AppName: "support", Limit: 10})
	if err != nil || len(page.Records) != 1 {
		t.Fatalf("records: %v", err)
	}
	got, _ := json.Marshal(page.Records[0].Detections)
	want, _ := json.Marshal(sensitiveSummary())
	if string(got) != string(want) {
		t.Fatalf("detections = %s, want %s", got, want)
	}
	var legacy storage.Record
	if err := json.Unmarshal([]byte(`{"RequestID":"x"}`), &legacy); err != nil || legacy.Detections != nil {
		t.Fatal("a record without detections must read as none")
	}
}
