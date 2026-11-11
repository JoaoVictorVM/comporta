package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/storage"
)

const forgottenHash = "11111111111111111111111111111111"
const keptHash = "22222222222222222222222222222222"

// materialize admits, completes and commits one record without limits.
func materialize(t *testing.T, s *Store, r storage.Record) {
	t.Helper()
	ctx := context.Background()
	if err := s.Admit(ctx, r); err != nil {
		t.Fatal(err)
	}
	done := completed(r)
	if err := s.Complete(ctx, done); err != nil {
		t.Fatal(err)
	}
	if err := s.Commit(ctx, []storage.Record{done}); err != nil {
		t.Fatal(err)
	}
}

// forgetHistory leaves a settled limited record and a plain record of the
// forgotten user in two apps, and a record of another user.
func forgetHistory(t *testing.T, s *Store) {
	t.Helper()
	m := limitManager(t, s)
	first := limitRecord(1)
	h, err := admitLimited(m, s, first)
	if err != nil {
		t.Fatal(err)
	}
	if err = settleCompleted(m, s, h, completed(first)); err != nil {
		t.Fatal(err)
	}
	if err = s.Commit(context.Background(), []storage.Record{completed(first)}); err != nil {
		t.Fatal(err)
	}
	other := limitRecord(2)
	other.AppName = "other"
	materialize(t, s, other)
	kept := limitRecord(3)
	kept.UserHash = keptHash
	materialize(t, s, kept)
	if err = m.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func subjects() []storage.SubjectRef {
	return []storage.SubjectRef{{App: "bot", Hash: forgottenHash}, {App: "other", Hash: forgottenHash}}
}

// mentions counts rows in every table that still carry the hash anywhere.
func mentions(t *testing.T, s *Store, hash string) int {
	t.Helper()
	rows, err := s.db.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var name string
		_ = rows.Scan(&name)
		tables = append(tables, name)
	}
	rows.Close()
	total := 0
	for _, table := range tables {
		columns, err := s.db.Query(`SELECT name FROM pragma_table_info(?)`, table)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for columns.Next() {
			var name string
			_ = columns.Scan(&name)
			names = append(names, "coalesce(CAST("+name+" AS TEXT),'')")
		}
		columns.Close()
		total += count(t, s, `SELECT count(*) FROM `+table+` WHERE instr(`+strings.Join(names, "||'|'||")+`,?)>0`, hash)
	}
	return total
}

func TestForgetSubjectsDeletesAndCounts(t *testing.T) {
	s := testStore(t)
	forgetHistory(t, s)
	appBefore := count(t, s, `SELECT sum(cost_microusd) FROM usage_daily WHERE scope='app'`)
	if mentions(t, s, forgottenHash) == 0 {
		t.Fatal("positive control: the user is not stored")
	}
	counts, err := s.ForgetSubjects(context.Background(), subjects())
	if err != nil {
		t.Fatal(err)
	}
	if counts.Requests != 2 || counts.UsageDaily != 2 || counts.Events != 2 || counts.Journal != 0 || counts.Unlinked < 1 {
		t.Fatalf("counts %+v", counts)
	}
	if counts.DeletedRows() != counts.Requests+counts.UsageDaily+counts.Events+counts.Journal+counts.BudgetState+counts.ReservationItems {
		t.Fatal("deleted_rows is not the sum of the categories")
	}
	if n := mentions(t, s, forgottenHash); n != 0 {
		t.Fatalf("%d rows still carry the forgotten user", n)
	}
	if count(t, s, `SELECT count(*) FROM requests WHERE user_hash=?`, keptHash) != 1 {
		t.Fatal("another user was deleted")
	}
	if after := count(t, s, `SELECT sum(cost_microusd) FROM usage_daily WHERE scope='app'`); after != appBefore {
		t.Fatalf("app totals changed: %d -> %d", appBefore, after)
	}
	if spent, _ := budget(t, limitManager(t, s)); spent != 49 {
		t.Fatalf("app limit spending after recovery: %d", spent)
	}
	again, err := s.ForgetSubjects(context.Background(), subjects())
	if err != nil || again.DeletedRows() != 0 || again.Unlinked != 0 {
		t.Fatalf("a repeated deletion: %+v %v", again, err)
	}
}

func TestForgetRollsBackAcrossApps(t *testing.T) {
	s := testStore(t)
	forgetHistory(t, s)
	before := mentions(t, s, forgottenHash)
	if _, err := s.db.Exec(`CREATE TRIGGER refuse BEFORE DELETE ON requests WHEN OLD.app_name='other' BEGIN SELECT RAISE(ABORT,'refused'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ForgetSubjects(context.Background(), subjects()); err == nil {
		t.Fatal("a failure in the second app was ignored")
	}
	if after := mentions(t, s, forgottenHash); after != before {
		t.Fatalf("a failed deletion removed %d rows", before-after)
	}
}

func TestForgetRefusesUsageInTransit(t *testing.T) {
	s := testStore(t)
	admitted := limitRecord(5)
	if err := s.Admit(context.Background(), admitted); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ForgetSubjects(context.Background(), subjects()); !errors.Is(err, storage.ErrBusy) {
		t.Fatalf("an admitted attempt without result: %v", err)
	}
	if count(t, s, `SELECT count(*) FROM metering_journal`) != 1 {
		t.Fatal("the journal entry was touched")
	}
	m := limitManager(t, s)
	if _, err := admitLimited(m, s, limitRecord(6)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ForgetSubjects(context.Background(), []storage.SubjectRef{{App: "bot", Hash: forgottenHash}}); !errors.Is(err, storage.ErrBusy) {
		t.Fatalf("an open reservation: %v", err)
	}
}

func lockDatabase(t *testing.T, s *Store) (release func()) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(s.dir, "usage.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(context.Background(), `BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	return func() {
		_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		_ = conn.Close()
		_ = db.Close()
	}
}

func TestForgetRetriesLockedDatabase(t *testing.T) {
	s := testStore(t)
	forgetHistory(t, s)
	release := lockDatabase(t, s)
	go func() {
		// Long enough for the first attempt to exhaust the busy timeout.
		time.Sleep(1500 * time.Millisecond)
		release()
	}()
	counts, err := s.ForgetSubjects(context.Background(), subjects())
	if err != nil || counts.Requests != 2 {
		t.Fatalf("after the lock was released: %+v %v", counts, err)
	}

	s2 := testStore(t)
	forgetHistory(t, s2)
	before := mentions(t, s2, forgottenHash)
	previous := forgetRetryWindow
	forgetRetryWindow = 300 * time.Millisecond
	defer func() { forgetRetryWindow = previous }()
	hold := lockDatabase(t, s2)
	defer hold()
	if _, err := s2.ForgetSubjects(context.Background(), subjects()); !errors.Is(err, storage.ErrUnavailable) {
		t.Fatalf("a lock held past the retry window: %v", err)
	}
	hold()
	if after := mentions(t, s2, forgottenHash); after != before {
		t.Fatal("a deletion that gave up removed rows")
	}
}

func TestHistoricalAppsIncludesEveryTable(t *testing.T) {
	s := testStore(t)
	forgetHistory(t, s)
	if _, _, err := s.SavePause(context.Background(), storage.PauseChange{App: "paused-only", Paused: true, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	apps, err := s.HistoricalApps(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(apps, ",") != "bot,other,paused-only" {
		t.Fatalf("apps %v", apps)
	}
}
