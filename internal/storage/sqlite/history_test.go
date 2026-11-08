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

func TestFutureSchemaCannotProveEmptyHistory(t *testing.T) {
	s := testStore(t)
	if _, err := s.db.Exec(`UPDATE schema_migrations SET version=99 WHERE version=(SELECT max(version) FROM schema_migrations)`); err != nil {
		t.Fatal(err)
	}
	state, err := s.Inspect(context.Background())
	if err == nil || state != useridentity.HistoryPresent {
		t.Fatal("future schema proved empty")
	}
	if reader, err := OpenReader(context.Background(), s.dir); err == nil {
		reader.Close()
		t.Fatal("future schema read as supported")
	}
}

func TestReadOnlyClosedDatabaseCreatesNothing(t *testing.T) {
	s := testStore(t)
	dir := s.dir
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := OpenReader(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if _, err = reader.Apps(context.Background()); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadDir(dir)
	if err != nil || len(before) != len(after) {
		t.Fatal("read-only query created SQLite artifacts")
	}
}

func TestUnknownRuntimeStateRefusesIdentityReset(t *testing.T) {
	s := testStore(t)
	if _, err := s.db.Exec(`INSERT INTO runtime_state VALUES('unknown','user','{"version":2}',0)`); err != nil {
		t.Fatal(err)
	}
	state, err := s.Inspect(context.Background())
	if err == nil || state != useridentity.HistoryPresent {
		t.Fatal("unknown runtime state proved empty")
	}
	if err = s.Reset(context.Background()); err == nil {
		t.Fatal("unknown runtime state reset without an owner")
	}
}

func userLimitRows(t *testing.T, s *Store) int {
	t.Helper()
	total := 0
	for _, query := range userLimitQueries {
		total += count(t, s, query)
	}
	return total
}

// limitHistory leaves settled, open and post-checkpoint reservations of one
// user, and returns the app spending they amount to after recovery.
func limitHistory(t *testing.T, s *Store) int64 {
	t.Helper()
	m := limitManager(t, s)
	first, third := limitRecord(1), limitRecord(3)
	h, err := admitLimited(m, s, first)
	if err != nil {
		t.Fatal(err)
	}
	if err = settleCompleted(m, s, h, completed(first)); err != nil {
		t.Fatal(err)
	}
	if _, err = admitLimited(m, s, limitRecord(2)); err != nil {
		t.Fatal(err)
	}
	if err = m.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	h, err = admitLimited(m, s, third)
	if err != nil {
		t.Fatal(err)
	}
	if err = settleCompleted(m, s, h, completed(third)); err != nil {
		t.Fatal(err)
	}
	return 49 + 100 + 49
}

func TestResetIncludesLimitsHistoryAndKeepsAppObligations(t *testing.T) {
	s := testStore(t)
	want := limitHistory(t, s)
	if state, err := s.Inspect(context.Background()); err != nil || state != useridentity.HistoryPresent {
		t.Fatalf("limit history not seen: %v %v", state, err)
	}
	if userLimitRows(t, s) == 0 {
		t.Fatal("positive control: no user-linked limit rows")
	}
	if err := s.Reset(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := userLimitRows(t, s); n != 0 {
		t.Fatalf("%d user-linked limit rows survived the reset", n)
	}
	m := limitManager(t, s)
	if spent, reserved := budget(t, m); spent != want || reserved != 0 {
		t.Fatalf("app obligations after reset: spent=%d reserved=%d", spent, reserved)
	}
	if userLimitRows(t, s) != 0 {
		t.Fatal("recovery linked a user again")
	}
}

func TestForgetLimitSubjectKeepsOtherUsersAndApp(t *testing.T) {
	s := testStore(t)
	want := limitHistory(t, s)
	other := limitRecord(9)
	other.UserHash = "22222222222222222222222222222222"
	m := limitManager(t, s)
	if _, err := admitLimited(m, s, other); err != nil {
		t.Fatal(err)
	}
	if err := m.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.ForgetLimitSubject(context.Background(), "bot", limitRecord(1).UserHash, false); err != nil {
		t.Fatal(err)
	}
	if count(t, s, `SELECT count(*) FROM limit_reservations WHERE subject=?`, limitRecord(1).UserHash) != 0 || count(t, s, `SELECT count(*) FROM budget_state WHERE subject=?`, limitRecord(1).UserHash) != 0 {
		t.Fatal("forgotten user still linked")
	}
	if count(t, s, `SELECT count(*) FROM limit_reservations WHERE subject=?`, other.UserHash) != 1 {
		t.Fatal("another user was forgotten too")
	}
	if spent, _ := budget(t, limitManager(t, s)); spent != want+100 {
		t.Fatalf("app spending changed: %d", spent)
	}
}

func TestResetCrashDoesNotRestoreUserLedger(t *testing.T) {
	dir := crashAt(t, "limits-reset")
	s, m, err := restartLimits(t, dir, nil)
	if err != nil {
		s.Close()
		t.Fatal(err)
	}
	defer s.Close()
	if n := userLimitRows(t, s); n != 0 {
		t.Fatalf("%d user-linked rows came back after a crash", n)
	}
	if spent, reserved := budget(t, m); spent != 198 || reserved != 0 {
		t.Fatalf("app obligations: spent=%d reserved=%d", spent, reserved)
	}
}
