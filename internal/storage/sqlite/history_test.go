package sqlite

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/storage"
	"github.com/JoaoVictorVM/comporta/internal/useridentity"
)

func TestPreviousReleaseWithoutDatabase(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "private")
	identity, err := useridentity.Open(ctx, useridentity.Options{Dir: dir})
	if err != nil || identity == nil {
		t.Fatal(err)
	}
	s, err := Prepare(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err = os.Stat(filepath.Join(dir, "usage.sqlite")); !os.IsNotExist(err) {
		t.Fatal("prepare created a database")
	}
	if err = s.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestResetKeepsAppSpend(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	r := fixture()
	if err := s.Admit(ctx, r); err != nil {
		t.Fatal(err)
	}
	state, err := s.Inspect(ctx)
	if err != nil || state != useridentity.HistoryPresent {
		t.Fatal("pending WAL history missed")
	}
	if err = s.Reset(ctx); err != nil {
		t.Fatal(err)
	}
	if err = s.Reset(ctx); err != nil {
		t.Fatal(err)
	}
	rows, err := s.Daily(ctx, storage.Filter{AppName: "bot"})
	if err != nil || len(rows) != 1 || rows[0].Cost != 100 {
		t.Fatalf("app spend changed: %+v %v", rows, err)
	}
	rows, err = s.Daily(ctx, storage.Filter{AppName: "bot", UserHash: r.UserHash})
	if err != nil || len(rows) != 0 {
		t.Fatal("user link survived reset")
	}
	if err = s.Recover(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	other, err := Prepare(ctx, s.dir)
	if err == nil {
		other.Close()
		t.Fatal("active instance lock not enforced")
	}
}

func TestUncertainHistoryFailsClosed(t *testing.T) {
	s := testStore(t)
	if _, err := s.db.Exec(`DROP TABLE budget_state`); err != nil {
		t.Fatal(err)
	}
	state, err := s.Inspect(context.Background())
	if err == nil || state != useridentity.HistoryPresent {
		t.Fatal("uncertain history treated as empty")
	}
}

func TestReadOnlyQueryCreatesNothing(t *testing.T) {
	dir := t.TempDir()
	if reader, err := OpenReader(context.Background(), dir); err == nil {
		reader.Close()
		t.Fatal("missing database opened")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatal("read-only query created files")
	}
}
