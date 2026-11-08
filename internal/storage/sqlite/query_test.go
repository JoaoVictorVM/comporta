package sqlite

import (
	"context"
	"testing"

	"github.com/JoaoVictorVM/comporta/internal/storage"
)

func TestHistoricalQueriesScopeAndStableCursor(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	first, second := fixture(), fixture()
	second.RequestID = "22222222222222222222222222222222"
	second.Anonymous = true
	for _, r := range []storage.Record{first, second} {
		if err := s.Admit(ctx, r); err != nil {
			t.Fatal(err)
		}
		if err := s.Commit(ctx, []storage.Record{r}); err != nil {
			t.Fatal(err)
		}
	}
	apps, err := s.Apps(ctx)
	if err != nil || len(apps) != 1 || apps[0] != "bot" {
		t.Fatal("historical apps missing")
	}
	page, err := s.Records(ctx, storage.RecordFilter{AppName: "bot", Limit: 1})
	if err != nil || len(page.Records) != 1 || page.Next == nil {
		t.Fatal("first cursor missing")
	}
	next, err := s.Records(ctx, storage.RecordFilter{AppName: "bot", Limit: 1, After: page.Next})
	if err != nil || len(next.Records) != 1 || next.Records[0].RequestID == page.Records[0].RequestID || next.Next != nil {
		t.Fatal("pagination repeated or lost records")
	}
	identified, err := s.Records(ctx, storage.RecordFilter{AppName: "bot", UserHash: first.UserHash})
	if err != nil || len(identified.Records) != 1 || identified.Records[0].Anonymous {
		t.Fatal("anonymous and identified scopes merged")
	}
	if _, err = s.Records(ctx, storage.RecordFilter{UserHash: first.UserHash}); err == nil {
		t.Fatal("unscoped user query accepted")
	}
}
