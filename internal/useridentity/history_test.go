package useridentity

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestNewDirectoryAllowsKeyCreation(t *testing.T) {
	state, err := NewFilesystemGuard(filepath.Join(t.TempDir(), "missing")).Inspect(context.Background())
	if err != nil || state != HistoryEmpty {
		t.Fatalf("state %v, %v", state, err)
	}
	state, err = NewFilesystemGuard(t.TempDir()).Inspect(context.Background())
	if err != nil || state != HistoryEmpty {
		t.Fatalf("empty dir: state %v, %v", state, err)
	}
}

func TestKeysOnlyDirectoryAllowsCreation(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"keys.json", "keys.lock", ".keys-1.tmp", ".hmac-0123.tmp"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writePrivate(t, filepath.Join(dir, lockFile), nil)
	state, err := NewFilesystemGuard(dir).Inspect(context.Background())
	if err != nil || state != HistoryEmpty {
		t.Fatalf("state %v, %v", state, err)
	}
	mustOpen(t, dir)
}

func TestUnknownArtifactsPreventCreation(t *testing.T) {
	cases := map[string]func(t *testing.T, dir string){
		"database": func(t *testing.T, dir string) { writeEmpty(t, dir, "comporta.db") },
		"wal":      func(t *testing.T, dir string) { writeEmpty(t, dir, "comporta.db-wal") },
		"shm":      func(t *testing.T, dir string) { writeEmpty(t, dir, "comporta.db-shm") },
		"unknown":  func(t *testing.T, dir string) { writeEmpty(t, dir, "notes.txt") },
		"keys.json with another case": func(t *testing.T, dir string) {
			writeEmpty(t, dir, "KEYS.JSON.bak")
		},
		"unrecognized temporary": func(t *testing.T, dir string) { writeEmpty(t, dir, ".other-1.tmp") },
		"subdirectory": func(t *testing.T, dir string) {
			if err := os.Mkdir(filepath.Join(dir, "backup"), 0o700); err != nil {
				t.Fatal(err)
			}
		},
		"directory named like a known file": func(t *testing.T, dir string) {
			if err := os.Mkdir(filepath.Join(dir, "keys.json"), 0o700); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			setup(t, dir)
			state, err := NewFilesystemGuard(dir).Inspect(context.Background())
			if err != nil || state != HistoryPresent {
				t.Fatalf("state %v, %v", state, err)
			}
			if _, err := Open(context.Background(), Options{Dir: dir}); !errors.Is(err, ErrHistoryExists) {
				t.Fatalf("open: %v", err)
			}
		})
	}
}

func writeEmpty(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestResetWithoutStorageNeverDeletesData(t *testing.T) {
	dir := t.TempDir()
	usage := filepath.Join(dir, "comporta.db")
	if err := os.WriteFile(usage, []byte("spend"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "keys.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Open(context.Background(), Options{Dir: dir, ResetHistory: true})
	if !errors.Is(err, ErrResetUnsupported) {
		t.Fatalf("got %v", err)
	}
	if err.Error() != "user history reset requires usage storage support; restore hmac.key or use a new data directory" {
		t.Fatalf("documented message changed: %s", err)
	}
	if data, err := os.ReadFile(usage); err != nil || string(data) != "spend" {
		t.Fatalf("usage data was touched: %q, %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "keys.json")); err != nil {
		t.Fatal("app keys were touched")
	}
	if _, err := os.Stat(filepath.Join(dir, keyFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("no key may be generated")
	}

	empty := t.TempDir()
	if _, err := Open(context.Background(), Options{Dir: empty, ResetHistory: true}); err != nil {
		t.Fatalf("with nothing to protect, the flag must not block: %v", err)
	}

	withKey := t.TempDir()
	first := mustOpen(t, withKey)
	again, err := Open(context.Background(), Options{Dir: withKey, ResetHistory: true})
	if err != nil {
		t.Fatal(err)
	}
	a, _ := first.LookupHash("bot", "user_123")
	b, _ := again.LookupHash("bot", "user_123")
	if a != b {
		t.Fatal("the flag rotated an existing key")
	}
}

type fakeGuard struct {
	state      HistoryState
	inspectErr error
	resetErr   error
	resets     int
}

func (g *fakeGuard) Inspect(context.Context) (HistoryState, error) { return g.state, g.inspectErr }
func (g *fakeGuard) Reset(context.Context) error {
	g.resets++
	return g.resetErr
}

func TestHistoryInspectionErrorIsNotEmpty(t *testing.T) {
	failure := errors.New("database is locked")
	dir := t.TempDir()
	guard := &fakeGuard{inspectErr: failure}
	if _, err := Open(context.Background(), Options{Dir: dir, History: guard}); !errors.Is(err, ErrHistoryExists) {
		t.Fatalf("got %v", err)
	}
	if _, err := Open(context.Background(), Options{Dir: dir, History: guard, ResetHistory: true}); !errors.Is(err, ErrResetUnsupported) {
		t.Fatalf("reset: got %v", err)
	}
	if guard.resets != 0 {
		t.Fatal("reset ran on a state that could not be inspected")
	}
	if _, err := os.Stat(filepath.Join(dir, keyFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("no key may be generated")
	}
}

func TestStorageAdapterContract(t *testing.T) {
	t.Run("a failed reset generates no key", func(t *testing.T) {
		dir := t.TempDir()
		guard := &fakeGuard{state: HistoryPresent, resetErr: errors.New("busy")}
		if _, err := Open(context.Background(), Options{Dir: dir, History: guard, ResetHistory: true}); err == nil {
			t.Fatal("expected a failure")
		}
		if _, err := os.Stat(filepath.Join(dir, keyFile)); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("no key may be generated")
		}
	})
	t.Run("a successful reset allows a key", func(t *testing.T) {
		dir := t.TempDir()
		guard := &fakeGuard{state: HistoryPresent}
		if _, err := Open(context.Background(), Options{Dir: dir, History: guard, ResetHistory: true}); err != nil {
			t.Fatal(err)
		}
		if guard.resets != 1 {
			t.Fatalf("resets %d", guard.resets)
		}
	})
	t.Run("no flag no reset", func(t *testing.T) {
		guard := &fakeGuard{state: HistoryPresent}
		if _, err := Open(context.Background(), Options{Dir: t.TempDir(), History: guard}); !errors.Is(err, ErrHistoryExists) {
			t.Fatalf("got %v", err)
		}
		if guard.resets != 0 {
			t.Fatal("reset without the flag")
		}
	})
}
