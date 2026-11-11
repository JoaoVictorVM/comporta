package admin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/storage"
	"github.com/JoaoVictorVM/comporta/internal/useridentity"
)

type usageSeed struct {
	f    *serviceFixture
	next int
	ids  *useridentity.Service
}

func newUsageSeed(t *testing.T, f *serviceFixture) *usageSeed {
	t.Helper()
	ids, err := useridentity.Open(context.Background(), useridentity.Options{Dir: f.manager.Capture().DataDir()})
	if err != nil {
		t.Fatal(err)
	}
	return &usageSeed{f: f, ids: ids}
}

// add commits one finished request of app and user, labeled with day and zone.
func (u *usageSeed) add(t *testing.T, app, user, day, zone string, cost int64) {
	t.Helper()
	u.next++
	hash, anonymous := u.ids.AnonymousHash(app).Hex(), true
	if user != "" {
		h, err := u.ids.LookupHash(app, user)
		if err != nil {
			t.Fatal(err)
		}
		hash, anonymous = h.Hex(), false
	}
	at := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	n := int64(5)
	r := storage.Record{RequestID: fmt.Sprintf("%032x", u.next), AttemptIndex: 1, StartedAt: at, FinishedAt: at, AppName: app, UserHash: hash, Anonymous: anonymous,
		Operation: "generation", Outcome: "ok", Input: &n, Output: &n, Cost: configMicrousd(cost), RequestCount: 1, Day: day, Timezone: zone}
	if err := u.f.store.Admit(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if err := u.f.store.Commit(context.Background(), []storage.Record{r}); err != nil {
		t.Fatal(err)
	}
}

func query(t *testing.T, s *Service, q UsageQuery) UsagePage {
	t.Helper()
	page, err := s.QueryUsage(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	return page
}

func TestUsageScopesNeverMix(t *testing.T) {
	f := newServiceAPI(t)
	f.clock.now = time.Date(2026, 11, 8, 15, 0, 0, 0, time.UTC)
	seed := newUsageSeed(t, f)
	seed.add(t, "other", "u1", "2026-11-08", "UTC", 100)
	seed.add(t, "other", "u1", "2026-11-08", "UTC", 100)
	seed.add(t, "other", "u2", "2026-11-08", "UTC", 50)
	seed.add(t, "other", "", "2026-11-08", "UTC", 7)
	app := query(t, f.service, UsageQuery{App: "other"})
	if app.Totals.Requests != "4" || app.Totals.CostMicrousd != "257" || app.Totals.Tokens != "40" || len(app.Rows) != 1 {
		t.Fatalf("app scope %+v", app)
	}
	user := query(t, f.service, UsageQuery{UserID: "u1", HasUser: true})
	if user.Totals.Requests != "2" || user.Totals.CostMicrousd != "200" {
		t.Fatalf("user scope across apps %+v", user)
	}
	anonymous := query(t, f.service, UsageQuery{App: "other", Anonymous: true})
	if anonymous.Totals.Requests != "1" || anonymous.Totals.CostMicrousd != "7" {
		t.Fatalf("anonymous bucket %+v", anonymous)
	}
	named := query(t, f.service, UsageQuery{App: "other", UserID: "anonymous", HasUser: true})
	if named.Totals.Requests != "0" || len(named.Rows) != 0 {
		t.Fatalf("a user named anonymous is not the bucket: %+v", named)
	}
	if page := query(t, f.service, UsageQuery{UserID: "nobody", HasUser: true}); page.Totals.Requests != "0" || page.Rows == nil || page.NextCursor != nil {
		t.Fatalf("empty result %+v", page)
	}
	if page := query(t, f.service, UsageQuery{}); page.Totals.Requests != "4" || page.ReferenceAt != "2026-11-08T15:00:00Z" || page.MaterializationDelaySeconds != 2 {
		t.Fatalf("every app %+v", page)
	}
}

func TestUsageSinceUsesEachAppTimezone(t *testing.T) {
	f := newServiceAPI(t)
	// At 02:00 UTC it is still November 7 in Sao Paulo.
	f.clock.now = time.Date(2026, 11, 8, 2, 0, 0, 0, time.UTC)
	seed := newUsageSeed(t, f)
	seed.add(t, "bot-discord", "u1", "2026-11-07", "America/Sao_Paulo", 1)
	seed.add(t, "bot-discord", "u1", "2026-11-08", "UTC", 10)
	seed.add(t, "other", "u1", "2026-11-08", "UTC", 100)
	seed.add(t, "removed-app", "u1", "2026-11-08", "Asia/Tokyo", 1000)
	seed.add(t, "removed-app", "u1", "2026-11-06", "Asia/Tokyo", 10000)
	if page := query(t, f.service, UsageQuery{Since: "1d"}); page.Totals.CostMicrousd != "1101" {
		t.Fatalf("since 1d: %+v", page)
	}
	if page := query(t, f.service, UsageQuery{From: "2026-11-08", Through: "2026-11-08"}); page.Totals.CostMicrousd != "1110" {
		t.Fatalf("labels are taken as recorded: %+v", page)
	}
	if page := query(t, f.service, UsageQuery{App: "removed-app", UserID: "u1", HasUser: true, Since: "3d"}); page.Totals.CostMicrousd != "11000" {
		t.Fatalf("a removed app keeps its history: %+v", page)
	}
}

func TestUsagePaginationAndCursor(t *testing.T) {
	f := newServiceAPI(t)
	f.clock.now = time.Date(2026, 11, 8, 15, 0, 0, 0, time.UTC)
	seed := newUsageSeed(t, f)
	for _, day := range []string{"2026-11-06", "2026-11-07", "2026-11-08"} {
		seed.add(t, "other", "u1", day, "UTC", 10)
	}
	var days []string
	q := UsageQuery{App: "other", Limit: 1}
	for i := 0; ; i++ {
		page := query(t, f.service, q)
		if page.Totals.Requests != "3" {
			t.Fatalf("totals must cover the whole filter: %+v", page.Totals)
		}
		for _, row := range page.Rows {
			days = append(days, row.Day)
		}
		if page.NextCursor == nil {
			break
		}
		if i == 0 {
			changed := q
			changed.Limit = 2
			changed.Cursor = *page.NextCursor
			if _, err := f.service.QueryUsage(context.Background(), changed); !errors.Is(err, ErrInvalidInput) {
				t.Fatal("a cursor reused with other filters was accepted")
			}
			tampered := q
			tampered.Cursor = *page.NextCursor + "x"
			if _, err := f.service.QueryUsage(context.Background(), tampered); !errors.Is(err, ErrInvalidInput) {
				t.Fatal("a tampered cursor was accepted")
			}
		}
		q.Cursor = *page.NextCursor
	}
	if strings.Join(days, ",") != "2026-11-06,2026-11-07,2026-11-08" {
		t.Fatalf("pages %v", days)
	}
	restarted := NewService(ServiceOptions{Control: f.control, Source: f.manager, Usage: f.store, Now: f.clock.Now})
	first := query(t, f.service, UsageQuery{App: "other", Limit: 1})
	if _, err := restarted.QueryUsage(context.Background(), UsageQuery{App: "other", Limit: 1, Cursor: *first.NextCursor}); !errors.Is(err, ErrInvalidInput) {
		t.Fatal("a cursor from another process was accepted")
	}
}

func TestUsageValidation(t *testing.T) {
	f := newServiceAPI(t)
	for _, q := range []UsageQuery{
		{Since: "367d"}, {Since: "0d"}, {Since: "7h"}, {From: "2025-01-01", Through: "2026-01-02"},
		{From: "2026-01-02", Through: "2026-01-01"}, {From: "2026-01-01"}, {Since: "7d", From: "2026-01-01", Through: "2026-01-01"},
		{UserID: "u1", HasUser: true, Anonymous: true}, {UserID: "a@b.example", HasUser: true}, {App: "Bad"}, {Limit: 1001}, {Limit: -1},
		{Cursor: strings.Repeat("a", 3000)},
	} {
		if _, err := f.service.QueryUsage(context.Background(), q); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("%+v accepted: %v", q, err)
		}
	}
	if _, err := f.service.QueryUsage(context.Background(), UsageQuery{From: "2025-01-01", Through: "2026-01-01"}); err != nil {
		t.Fatalf("366 days: %v", err)
	}
	response := f.do(call{method: http.MethodPost, target: "/admin/v1/usage/query", body: `{"user_id":"cmp-marker-77","since":"999d"}`, header: f.bearer()})
	if response.Code != http.StatusBadRequest || strings.Contains(response.Body.String(), "cmp-marker-77") {
		t.Fatalf("%d %s", response.Code, response.Body)
	}
	response = f.do(call{method: http.MethodPost, target: "/admin/v1/usage/query", body: `{"app":"other","since":"7d","limit":100}`, header: f.bearer()})
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"next_cursor":null`) || !strings.Contains(response.Body.String(), `"rows":[]`) {
		t.Fatalf("%d %s", response.Code, response.Body)
	}
}
