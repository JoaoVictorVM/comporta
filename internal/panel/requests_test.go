package panel

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/sensitivedata"
	"github.com/JoaoVictorVM/comporta/internal/storage"
)

func TestRequestFiltersAndDescendingCursor(t *testing.T) {
	env := newEnv(t)
	at := time.Date(2026, 11, 18, 15, 0, 0, 0, time.UTC)
	var records []storage.Record
	for n := 1; n <= 120; n++ {
		user := "alice"
		if n%2 == 0 {
			user = "bob"
		}
		records = append(records, env.record(t, n, "bot", user, at.Add(time.Duration(n)*time.Second), 10))
	}
	env.commit(t, records...)
	list, err := env.service.Requests(context.Background(), RequestQuery{})
	if err != nil || len(list.Rows) != storage.PanelPageSize || list.Next == "" || list.Rows[0].RequestID != records[119].RequestID {
		t.Fatalf("first page: %v %d %q", err, len(list.Rows), list.Next)
	}
	for i := 1; i < len(list.Rows); i++ {
		if list.Rows[i].StartedAt.After(list.Rows[i-1].StartedAt) {
			t.Fatal("rows are not newest first")
		}
	}
	second, err := env.service.Requests(context.Background(), RequestQuery{Cursor: list.Next})
	if err != nil || len(second.Rows) != storage.PanelPageSize || second.Shown != 100 || second.Rows[0].RequestID != records[69].RequestID {
		t.Fatalf("second page: %v %d %d", err, len(second.Rows), second.Shown)
	}
	// A cursor issued under other filters is refused.
	if r, _ := env.service.Requests(context.Background(), RequestQuery{App: "bot", Cursor: list.Next}); !r.Invalid {
		t.Fatal("a cursor crossed filters")
	}
	if r, _ := env.service.Requests(context.Background(), RequestQuery{Cursor: list.Next[:len(list.Next)-2] + "AA"}); !r.Invalid {
		t.Fatal("a forged cursor was accepted")
	}
	bob, err := env.service.Requests(context.Background(), RequestQuery{App: "bot", UserID: "bob"})
	if err != nil || len(bob.Rows) != 50 || bob.Filters.UserHash == "" {
		t.Fatalf("user filter: %v %+v", err, bob.Filters)
	}
	hash, _ := env.identity.LookupHash("bot", "bob")
	for _, row := range bob.Rows {
		if row.User != ShortHash(hash.Hex()) {
			t.Fatalf("row of another user: %+v", row)
		}
	}
	byHash, _ := env.service.Requests(context.Background(), RequestQuery{App: "bot", UserHash: hash.Hex()})
	if len(byHash.Rows) != 50 || byHash.Rows[0].RequestID != bob.Rows[0].RequestID {
		t.Fatal("a displayed pseudonym selects the same rows")
	}
	for _, q := range []RequestQuery{{UserID: "bob"}, {Outcome: "deleted"}, {App: "Bot!"}, {Model: "a b"}, {App: "bot", UserID: "x@y.z"}} {
		if r, _ := env.service.Requests(context.Background(), q); !r.Invalid {
			t.Fatalf("%+v accepted", q)
		}
	}
	// Five hundred rows at most, then the list says it is capped.
	more := make([]storage.Record, 0, 450)
	for n := 121; n <= 570; n++ {
		more = append(more, env.record(t, n, "web", "carol", at.Add(time.Duration(n)*time.Second), 10))
	}
	env.commit(t, more...)
	q := RequestQuery{}
	total := 0
	var last RequestList
	for range 20 {
		last, err = env.service.Requests(context.Background(), q)
		if err != nil || last.Invalid {
			t.Fatal(err)
		}
		total += len(last.Rows)
		if last.Next == "" {
			break
		}
		q.Cursor = last.Next
	}
	if total != storage.PanelMaxRequests || !last.Capped {
		t.Fatalf("showed %d rows, capped %v", total, last.Capped)
	}
}

// TestRequestDTOAllowlist fails when a field is added to the projection
// without a review of what it may expose.
func TestRequestDTOAllowlist(t *testing.T) {
	var fields []string
	for _, f := range reflect.VisibleFields(reflect.TypeOf(RequestRow{})) {
		fields = append(fields, f.Name)
	}
	want := "RequestID Attempt StartedAt App User Anonymous Provider ModelRequested ModelUsed Status Outcome Policy Cost CostKnown InputTokens OutputTokens LatencyMS TTFBMS Flags"
	if strings.Join(fields, " ") != want {
		t.Fatalf("request row fields changed: %v", fields)
	}
}

func TestDetailNeverContainsContent(t *testing.T) {
	env := newEnv(t)
	r := env.record(t, 1, "bot", "private-user-id", time.Date(2026, 11, 18, 15, 0, 0, 0, time.UTC), 1_200)
	r.Detections = sensitivedata.Summary{{DataType: sensitivedata.Email, Count: 2, Action: sensitivedata.ModeRedact}}
	r.Quote.EstimateMethod = "chars"
	r.ErrorClass, r.ErrorCode = "upstream_5xx", "provider_error"
	env.commit(t, r)
	d, err := env.service.Request(context.Background(), r.RequestID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Detections) != 1 || d.Detections[0].Category != "email" || d.Detections[0].Count != 2 || d.ErrorCode != "provider_error" {
		t.Fatalf("detail: %+v", d)
	}
	data, _ := json.Marshal(d)
	for _, forbidden := range []string{"private-user-id", r.UserHash, "localhost:12345"} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("detail exposes %q", forbidden)
		}
	}
	if _, err := env.service.Request(context.Background(), strings.Repeat("0", 32), 1); err != ErrNotFound {
		t.Fatalf("missing request: %v", err)
	}
	if _, err := env.service.Request(context.Background(), "../etc", 1); err != ErrNotFound {
		t.Fatal("an invalid id reached storage")
	}
}
