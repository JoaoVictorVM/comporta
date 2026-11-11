package sqlite

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/events"
	"github.com/JoaoVictorVM/comporta/internal/storage"
	"github.com/JoaoVictorVM/comporta/internal/useridentity"
)

func TestRuntimeAcceptsAdminNamespaces(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	initialized, err := s.AdminInitialized(ctx)
	if err != nil || initialized {
		t.Fatalf("fresh database: %v %v", initialized, err)
	}
	if err := s.MarkAdminInitialized(ctx); err != nil {
		t.Fatal(err)
	}
	if initialized, err = s.AdminInitialized(ctx); err != nil || !initialized {
		t.Fatalf("after marking: %v %v", initialized, err)
	}
	for _, name := range []string{"admin-token.json", "admin-token.lock", ".admin-token-0123456789abcdef0123456789abcdef.tmp"} {
		if err := os.WriteFile(filepath.Join(s.dir, name), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if state, err := s.Inspect(ctx); err != nil || state != useridentity.HistoryEmpty {
		t.Fatalf("admin artifacts must not count as history: %v %v", state, err)
	}
	if err := s.Reset(ctx); err != nil {
		t.Fatalf("an identity reset must accept admin artifacts: %v", err)
	}
	if initialized, err = s.AdminInitialized(ctx); err != nil || !initialized {
		t.Fatal("an identity reset must keep the admin marker")
	}

	for _, rejected := range []struct{ namespace, key, value string }{
		{"admin", "token_initialized", `{"version":2,"initialized":true}`},
		{"admin", "token", `{"version":1,"initialized":true}`},
		{"admin", "token_initialized", `{"version":1,"initialized":true,"token":"x"}`},
		{"pause_app", "Bad App", `{"version":1,"paused":true}`},
		{"pause_app", "bot", `{"version":1}`},
		{"pause_global", "other", `{"version":1,"paused":true}`},
	} {
		if _, err := s.db.Exec(`DELETE FROM runtime_state WHERE namespace<>'identity'`); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`INSERT INTO runtime_state VALUES(?,?,?,0)`, rejected.namespace, rejected.key, rejected.value); err != nil {
			t.Fatal(err)
		}
		if err := s.inspectRuntime(ctx); err == nil {
			t.Fatalf("accepted %+v", rejected)
		}
	}
	if err := os.WriteFile(filepath.Join(s.dir, ".admin-token-x.tmp"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DELETE FROM runtime_state WHERE namespace<>'identity'`); err != nil {
		t.Fatal(err)
	}
	if state, _ := s.Inspect(ctx); state != useridentity.HistoryPresent {
		t.Fatal("an unrecognized temporary file made the directory look empty")
	}
}

func pauseEvents(t *testing.T, s *Store) []string {
	t.Helper()
	rows, err := s.db.Query(`SELECT coalesce(app_name,''),payload_json FROM events WHERE type='paused' ORDER BY occurred_at,rowid`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var app, payload string
		if err := rows.Scan(&app, &payload); err != nil {
			t.Fatal(err)
		}
		out = append(out, app+" "+payload)
	}
	return out
}

func TestPauseAndEventCommitTogether(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	at := time.Date(2026, 11, 8, 6, 12, 44, 0, time.UTC)
	var notified []events.Event
	sub := s.Bus.Subscribe(ctx, events.Paused)
	state, changed, err := s.SavePause(ctx, storage.PauseChange{App: "bot-discord", Paused: true, At: at})
	if err != nil || !changed || len(state.Apps) != 1 || !state.Apps[0].Paused || !state.Apps[0].UpdatedAt.Equal(at) {
		t.Fatalf("pause: %+v %v %v", state, changed, err)
	}
	if _, changed, err = s.SavePause(ctx, storage.PauseChange{App: "bot-discord", Paused: true, At: at.Add(time.Second)}); err != nil || changed {
		t.Fatal("a repeated pause must not change anything")
	}
	if _, changed, err = s.SavePause(ctx, storage.PauseChange{App: "bot-discord", Paused: false, At: at.Add(2 * time.Second)}); err != nil || !changed {
		t.Fatal("resume")
	}
	if _, changed, err = s.SavePause(ctx, storage.PauseChange{All: true, Paused: true, At: at.Add(3 * time.Second)}); err != nil || !changed {
		t.Fatal("pause all")
	}
	got := pauseEvents(t, s)
	want := []string{`bot-discord {"paused":true,"scope":"app"}`, `bot-discord {"paused":false,"scope":"app"}`, ` {"paused":true,"scope":"global"}`}
	if len(got) != len(want) {
		t.Fatalf("events %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event %d: %s", i, got[i])
		}
	}
	for len(notified) < 3 {
		select {
		case e := <-sub.Events:
			notified = append(notified, e)
		case <-time.After(time.Second):
			t.Fatalf("bus received %d events", len(notified))
		}
	}
	if *notified[1].Payload.Paused {
		t.Fatal("a resume must be announced with paused:false")
	}
	page, err := s.ReadEvents(ctx, events.Filter{Types: []events.Type{events.Paused}})
	if err != nil || len(page.Events) != 3 {
		t.Fatalf("reading pause events back: %v %d", err, len(page.Events))
	}

	if _, _, err := s.SavePause(ctx, storage.PauseChange{App: "bot", All: true, Paused: true, At: at}); err == nil {
		t.Fatal("two selectors accepted")
	}
	if _, err := s.db.Exec(`CREATE TRIGGER refuse BEFORE INSERT ON events WHEN NEW.type='paused' BEGIN SELECT RAISE(ABORT,'refused'); END`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.SavePause(ctx, storage.PauseChange{App: "other", Paused: true, At: at}); err == nil {
		t.Fatal("a failed event insert committed the pause")
	}
	loaded, err := s.LoadPause(ctx)
	if err != nil || len(loaded.Apps) != 0 || !loaded.Global {
		t.Fatalf("state after a rolled back pause: %+v %v", loaded, err)
	}
	if err := s.inspectRuntime(ctx); err != nil {
		t.Fatalf("pause namespaces must pass inspection: %v", err)
	}
}
