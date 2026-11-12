package sqlite

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/events"
	"github.com/JoaoVictorVM/comporta/internal/storage"
)

const loopHash = "33333333333333333333333333333333"

func loopCut(n int, eventID string) storage.LocalCut {
	now := time.Date(2026, 11, 11, 15, 0, 0, 0, time.UTC)
	status := 429
	r := storage.Record{RequestID: fmt.Sprintf("%032x", n), AttemptIndex: 0, StartedAt: now, FinishedAt: now, AppName: "agent", UserHash: loopHash,
		Operation: "generation", ModelRequested: "m", Outcome: "loop_cut", ErrorClass: "rate_limit", ErrorCode: "loop_detected",
		DownstreamStatus: &status, Day: "2026-11-11", Timezone: "UTC"}
	e := events.NewLoopCut(eventID, now, fmt.Sprintf("%032x", 1), "agent", loopHash, false, events.ReasonSimilarRequests, 11, 30*time.Second, 120*time.Second)
	return storage.LocalCut{Record: r, Event: e}
}

const episodeID = "11111111111111111111111111111111"

func TestLocalCutAndEventAtomic(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.CommitLocalCut(ctx, loopCut(1, episodeID)); err != nil {
		t.Fatal(err)
	}
	if count(t, s, `SELECT count(*) FROM requests WHERE outcome='loop_cut' AND attempt_index=0 AND cost_microusd=0 AND reserved_cost_microusd=0 AND request_count=0`) != 1 {
		t.Fatal("cut record missing")
	}
	if count(t, s, `SELECT count(*) FROM events WHERE type='loop_cut' AND user_hash=? AND anonymous=0`, loopHash) != 1 {
		t.Fatal("event without its subject columns")
	}
	if count(t, s, `SELECT count(*) FROM metering_journal`)+count(t, s, `SELECT count(*) FROM usage_daily`)+count(t, s, `SELECT count(*) FROM limit_reservations`) != 0 {
		t.Fatal("a cut must not touch journal, aggregates or reservations")
	}
	// The event conflicts with a stored one: neither row of the cut stays.
	bad := loopCut(2, episodeID)
	bad.Event.Payload.Count = 12
	if err := s.CommitLocalCut(ctx, bad); err != storage.ErrConflict {
		t.Fatalf("conflicting event: %v", err)
	}
	if count(t, s, `SELECT count(*) FROM requests`) != 1 {
		t.Fatal("record committed without its event")
	}
}

func TestCutCommitIdempotent(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for range 3 {
		if err := s.CommitLocalCut(ctx, loopCut(1, episodeID)); err != nil {
			t.Fatal(err)
		}
	}
	// A retry in the same episode carries the same event.
	if err := s.CommitLocalCut(ctx, loopCut(2, episodeID)); err != nil {
		t.Fatal(err)
	}
	if count(t, s, `SELECT count(*) FROM requests`) != 2 || count(t, s, `SELECT count(*) FROM events`) != 1 {
		t.Fatal("duplicated rows")
	}
	changed := loopCut(1, episodeID)
	changed.Record.ModelRequested = "other"
	if err := s.CommitLocalCut(ctx, changed); err != storage.ErrConflict {
		t.Fatalf("conflicting record: %v", err)
	}
}

func TestLoopEventNotifiedAfterCommit(t *testing.T) {
	s := testStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sub := s.Bus.Subscribe(ctx)
	if err := s.CommitLocalCut(ctx, loopCut(1, episodeID)); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-sub.Events:
		if got.Type != events.LoopCut || got.UserHash != loopHash || got.Payload.WindowMS != 30000 {
			t.Fatalf("notified event: %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("no notification")
	}
	if err := s.CommitLocalCut(ctx, loopCut(2, episodeID)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-sub.Events:
		t.Fatal("a repeated episode notified again")
	default:
	}
}

func TestRollbackNotNotified(t *testing.T) {
	s := testStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sub := s.Bus.Subscribe(ctx)
	if err := s.CommitLocalCut(ctx, loopCut(1, episodeID)); err != nil {
		t.Fatal(err)
	}
	<-sub.Events
	// A commit that fails after the event row was prepared must stay silent.
	other := loopCut(1, "22222222222222222222222222222222")
	other.Record.ModelRequested = "changed"
	if err := s.CommitLocalCut(ctx, other); err == nil {
		t.Fatal("expected a conflict")
	}
	select {
	case <-sub.Events:
		t.Fatal("rolled-back event notified")
	default:
	}
	if count(t, s, `SELECT count(*) FROM events`) != 1 {
		t.Fatal("rolled-back event persisted")
	}
}

func TestLoopRecordDoesNotAggregateAttempt(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.CommitLocalCut(ctx, loopCut(1, episodeID)); err != nil {
		t.Fatal(err)
	}
	daily, err := s.Daily(ctx, storage.Filter{AppName: "agent", Since: "2026-11-01", Until: "2026-11-30"})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range daily {
		if d.Requests != 0 || d.Attempts != 0 || d.Cost != 0 {
			t.Fatalf("a cut was aggregated: %+v", d)
		}
	}
	// The cut is still visible to the history queries.
	page, err := s.Records(ctx, storage.RecordFilter{AppName: "agent", Limit: 10})
	if err != nil || len(page.Records) != 1 || page.Records[0].Outcome != "loop_cut" {
		t.Fatalf("history: %+v %v", page, err)
	}
	// The batch path refuses a loop cut, which would gain a blocked event.
	if err := s.Commit(ctx, []storage.Record{loopCut(2, episodeID).Record}); err != storage.ErrInvalid {
		t.Fatalf("batch commit of a cut: %v", err)
	}
}

func TestLoopEventSubjectRoundTrip(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.CommitLocalCut(ctx, loopCut(1, episodeID)); err != nil {
		t.Fatal(err)
	}
	page, err := s.ReadEvents(ctx, events.Filter{Types: []events.Type{events.LoopCut}})
	if err != nil || len(page.Events) != 1 {
		t.Fatalf("read: %v", err)
	}
	got := page.Events[0]
	if got.UserHash != loopHash || got.Anonymous == nil || *got.Anonymous || got.Payload.Reason != events.ReasonSimilarRequests || got.Payload.CooldownMS != 120000 {
		t.Fatalf("round trip: %+v", got)
	}
	// An older loop event without subject columns stays readable.
	if _, err := s.db.Exec(`INSERT INTO events(event_id,type,occurred_at,app_name,payload_json) VALUES(?,?,?,?,?)`, "44444444444444444444444444444444", "loop_cut", time.Now().UnixMicro(), "agent", `{"outcome":"loop_cut","count":11}`); err != nil {
		t.Fatal(err)
	}
	if page, err = s.ReadEvents(ctx, events.Filter{Types: []events.Type{events.LoopCut}}); err != nil || len(page.Events) != 2 {
		t.Fatalf("legacy event: %v", err)
	}
}

func TestLoopEventIdempotenceIncludesSubject(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	cut := loopCut(1, episodeID)
	if err := s.Publish(ctx, cut.Event); err != nil {
		t.Fatal(err)
	}
	flipped := cut.Event.Clone()
	*flipped.Anonymous = true
	if err := s.Publish(ctx, flipped); err != storage.ErrConflict {
		t.Fatalf("same id with another subject: %v", err)
	}
	other := cut.Event
	other.UserHash = "55555555555555555555555555555555"
	if err := s.Publish(ctx, other); err != storage.ErrConflict {
		t.Fatalf("same id with another hash: %v", err)
	}
}

func TestForgetLoopEventWithoutRequest(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.CommitLocalCut(ctx, loopCut(1, episodeID)); err != nil {
		t.Fatal(err)
	}
	// Retention removed the request; the event alone still names the user.
	if _, err := s.db.Exec(`DELETE FROM requests`); err != nil {
		t.Fatal(err)
	}
	counts, err := s.ForgetSubjects(ctx, []storage.SubjectRef{{App: "agent", Hash: loopHash}})
	if err != nil || counts.Events != 1 {
		t.Fatalf("forget: %+v %v", counts, err)
	}
	if mentions(t, s, loopHash) != 0 {
		t.Fatal("the user is still mentioned")
	}
}
