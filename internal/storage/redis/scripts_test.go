//go:build distributed

package redis

import (
	"context"
	"errors"
	"math"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/limits"
	"github.com/JoaoVictorVM/comporta/internal/storage"
	"github.com/JoaoVictorVM/comporta/internal/testkit"
)

func TestMain(m *testing.M) { os.Exit(testkit.RunWithDatabases(m)) }

const userHash = "11111111111111111111111111111111"

func openTest(t *testing.T) *Store {
	t.Helper()
	addr, db := testkit.Redis(t)
	s, err := Open(context.Background(), Options{Address: addr, Database: db})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	s.UseEpoch("test-"+strconv.Itoa(db), "00000000000000000000000000000001")
	if err := s.Client().FlushDB(context.Background()).Err(); err != nil {
		t.Fatal(err)
	}
	if err := s.Publish(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	return s
}

func plan(request string, amount int64) limits.Plan {
	day := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	start, end := day.UnixMicro(), day.AddDate(0, 0, 1).UnixMicro()
	at := day.Add(12 * time.Hour)
	return limits.Plan{Slot: true, Reservation: storage.Reservation{RequestID: request, AttemptIndex: 1, AppName: "bot", Subject: userHash, AdmittedAt: at, State: storage.ReservationReserved,
		Quote: storage.LimitQuote{Operation: "generation", EstimatedTokens: 10, ReservedCost: 0},
		Items: []storage.LimitItem{
			{Scope: "app", Dimension: "usd_per_day", WindowStart: start, WindowEnd: end, Timezone: "UTC", Reserved: amount},
			{Scope: "app", Dimension: "tokens_per_day", WindowStart: start, WindowEnd: end, Timezone: "UTC", Reserved: 10},
			{Scope: "user", Subject: userHash, Dimension: "usd_per_day", WindowStart: start, WindowEnd: end, Timezone: "UTC", Reserved: amount},
		},
		Rate: &storage.RateEvent{RequestID: request, AttemptIndex: 1, AppName: "bot", Subject: userHash, AdmittedAt: at, EstimatedTokens: 10, Active: true}}}
}

func reserved(t *testing.T, s *Store, scope string) map[string]int64 {
	t.Helper()
	snap, err := s.Read(context.Background(), "bot", limits.Subject{App: "bot", Hash: userHash})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]int64{}
	for _, b := range snap.State.Buckets {
		if b.Scope == scope {
			out[b.Dimension] = b.Reserved
			out[b.Dimension+"/spent"] = b.Spent
		}
	}
	return out
}

func admit(t *testing.T, s *Store, p limits.Plan, version, digest string) AdmitStatus {
	t.Helper()
	status, err := s.Admit(context.Background(), Admission{Plan: p, Version: version, Digest: digest, Owner: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"})
	if err != nil {
		t.Fatal(err)
	}
	return status
}

func TestAdmitAllDimensionsAtomic(t *testing.T) {
	s := openTest(t)
	if status := admit(t, s, plan("0000000000000000000000000000000a", 500), "0", "d1"); status != AdmitOK {
		t.Fatal(status)
	}
	app, user := reserved(t, s, "app"), reserved(t, s, "user")
	if app["usd_per_day"] != 500 || app["tokens_per_day"] != 10 || user["usd_per_day"] != 500 {
		t.Fatalf("every dimension must be reserved together: %v %v", app, user)
	}
	snap, _ := s.Read(context.Background(), "bot", limits.Subject{App: "bot", Hash: userHash})
	if snap.State.Concurrent[limits.Subject{App: "bot"}] != 1 || len(snap.State.Rates[limits.Subject{App: "bot"}]) != 1 {
		t.Fatal("slot and rate entry must be written with the reservation")
	}
	// A plan decided on an older version writes nothing at all.
	if status := admit(t, s, plan("0000000000000000000000000000000b", 700), "0", "d2"); status != AdmitStale {
		t.Fatalf("expected stale, got %s", status)
	}
	if reserved(t, s, "app")["usd_per_day"] != 500 {
		t.Fatal("a stale plan changed the projection")
	}
}

func TestCommandRetrySameHandle(t *testing.T) {
	s := openTest(t)
	p := plan("0000000000000000000000000000000c", 300)
	if status := admit(t, s, p, "0", "same"); status != AdmitOK {
		t.Fatal(status)
	}
	// A lost reply is resent with the same plan: confirmed, not applied twice.
	if status := admit(t, s, p, "0", "same"); status != AdmitDuplicate {
		t.Fatalf("expected a confirmation, got %s", status)
	}
	if status := admit(t, s, p, "1", "other"); status != AdmitConflict {
		t.Fatalf("a different payload with the same key must conflict, got %s", status)
	}
	if reserved(t, s, "app")["usd_per_day"] != 300 {
		t.Fatal("retry charged twice")
	}
	tokens := int64(7)
	ending := Ending{Reservation: p.Reservation, Digest: "end", Settlement: storage.Settlement{RequestID: p.Reservation.RequestID, AttemptIndex: 1,
		Transition: storage.TransitionSettle, Settled: []int64{120, 7, 120}, RateTokens: &tokens}}
	for i, want := range []SettleStatus{SettleOK, SettleDup} {
		status, err := s.Settle(context.Background(), ending)
		if err != nil || status != want {
			t.Fatalf("settle %d: %s %v", i, status, err)
		}
	}
	app := reserved(t, s, "app")
	if app["usd_per_day"] != 0 || app["usd_per_day/spent"] != 120 || app["tokens_per_day/spent"] != 7 {
		t.Fatalf("settlement applied wrongly: %v", app)
	}
	unknown := ending
	unknown.Reservation.RequestID, unknown.Settlement.RequestID = "0000000000000000000000000000000d", "0000000000000000000000000000000d"
	if status, err := s.Settle(context.Background(), unknown); err != nil || status != SettleUnknown {
		t.Fatalf("a settlement of an admission never seen must not subtract anything: %s %v", status, err)
	}
}

func TestScriptPartialWriteFencesEpoch(t *testing.T) {
	s := openTest(t)
	k := s.Keys()
	field := BucketField("usd_per_day", plan("x", 0).Reservation.Items[2].WindowStart, plan("x", 0).Reservation.Items[2].WindowEnd)
	// Corrupt the last bucket the plan touches: validation runs over every item
	// before the first write, so the earlier items stay untouched.
	if err := s.Client().HSet(context.Background(), k.Budget("bot", SubjectKey(userHash, false)), field+"|r", "not-a-number").Err(); err != nil {
		t.Fatal(err)
	}
	if status, err := s.Admit(context.Background(), Admission{Plan: plan("0000000000000000000000000000000e", 100), Version: "0", Digest: "d", Owner: "o"}); err == nil && (status == AdmitOK || status == AdmitDuplicate) {
		t.Fatal("a corrupt bucket must refuse the plan")
	} else if err != nil && !errors.Is(err, ErrIntegrity) {
		t.Fatalf("corruption is an integrity failure, got %v", err)
	}
	fields, _ := s.Client().HGetAll(context.Background(), k.Budget("bot", AppSubject)).Result()
	if len(fields) != 0 {
		t.Fatalf("a refused plan left a partial write: %v", fields)
	}
	if v, _ := s.Client().Get(context.Background(), k.Version("bot")).Result(); v != "" {
		t.Fatal("version moved on a refused plan")
	}
	// A closed epoch refuses every script.
	if err := s.Fence(context.Background(), "test-"+strconv.Itoa(0), "none"); err != nil {
		t.Fatal(err)
	}
	cluster := s.Keys().Prefix()
	_ = cluster
	if err := s.Client().HSet(context.Background(), k.Meta(), "state", "closed").Err(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Admit(context.Background(), Admission{Plan: plan("0000000000000000000000000000000f", 1), Version: "0", Digest: "d", Owner: "o"}); !errors.Is(err, ErrFenced) {
		t.Fatalf("a closed epoch must fence admissions, got %v", err)
	}
}

func TestIntegerAboveTwoTo53(t *testing.T) {
	s := openTest(t)
	big := int64(1<<53) + 1
	p := plan("00000000000000000000000000000010", big)
	if status := admit(t, s, p, "0", "d"); status != AdmitOK {
		t.Fatal(status)
	}
	if got := reserved(t, s, "app")["usd_per_day"]; got != big {
		t.Fatalf("value above 2^53 lost precision: %d != %d", got, big)
	}
	ending := Ending{Reservation: p.Reservation, Digest: "e", Settlement: storage.Settlement{RequestID: p.Reservation.RequestID, AttemptIndex: 1,
		Transition: storage.TransitionSettle, Settled: []int64{big + 2, 10, big + 2}}}
	if status, err := s.Settle(context.Background(), ending); err != nil || status != SettleOK {
		t.Fatal(status, err)
	}
	app := reserved(t, s, "app")
	if app["usd_per_day"] != 0 || app["usd_per_day/spent"] != big+2 {
		t.Fatalf("exact arithmetic failed: %v", app)
	}
}

func TestMaxInt64OverflowRejected(t *testing.T) {
	s := openTest(t)
	if status := admit(t, s, plan("00000000000000000000000000000011", math.MaxInt64-5), "0", "d"); status != AdmitOK {
		t.Fatal(status)
	}
	status, err := s.Admit(context.Background(), Admission{Plan: plan("00000000000000000000000000000012", 6), Version: "1", Digest: "x", Owner: "o"})
	if err == nil || !errors.Is(err, ErrIntegrity) {
		t.Fatalf("an overflow must refuse without writing, got %s %v", status, err)
	}
	if reserved(t, s, "app")["usd_per_day"] != math.MaxInt64-5 {
		t.Fatal("overflow changed the bucket")
	}
	if status := admit(t, s, plan("00000000000000000000000000000013", 5), "1", "y"); status != AdmitOK {
		t.Fatal("landing exactly on MaxInt64 is valid")
	}
}

func TestReleaseSlotAndRollback(t *testing.T) {
	s := openTest(t)
	p := plan("00000000000000000000000000000014", 40)
	admit(t, s, p, "0", "r")
	if err := s.ReleaseSlot(context.Background(), "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bot", limits.Subject{App: "bot", Hash: userHash}, p.Reservation.RequestID); err != nil {
		t.Fatal(err)
	}
	if err := s.ReleaseSlot(context.Background(), "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bot", limits.Subject{App: "bot", Hash: userHash}, p.Reservation.RequestID); err != nil {
		t.Fatal("releasing twice must be harmless")
	}
	snap, _ := s.Read(context.Background(), "bot", limits.Subject{App: "bot", Hash: userHash})
	if snap.State.Concurrent[limits.Subject{App: "bot"}] != 0 {
		t.Fatal("slot not released")
	}
	if err := s.Rollback(context.Background(), "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "r", p.Reservation); err != nil {
		t.Fatal(err)
	}
	if reserved(t, s, "app")["usd_per_day"] != 0 {
		t.Fatal("rollback must return the reservation")
	}
	if err := s.Rollback(context.Background(), "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "r", p.Reservation); err != nil {
		t.Fatal("a second rollback is skipped")
	}
	if reserved(t, s, "app")["usd_per_day"] != 0 {
		t.Fatal("rollback applied twice")
	}
}
