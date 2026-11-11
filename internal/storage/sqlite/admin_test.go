package sqlite

import (
	"context"
	"os"
	"path/filepath"
	"testing"

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
