//go:build !windows

package keys

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestStorePrivateModes(t *testing.T) {
	store := newTestStore(t)
	mustCreate(t, store, "bot")

	info, err := os.Stat(store.dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("new directory mode %o", got)
	}
	for _, path := range []string{store.documentPath(), store.lockPath()} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("%s mode %o", filepath.Base(path), got)
		}
	}
	mustCreate(t, store, "bot")
	info, err = os.Stat(store.documentPath())
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("replacement mode %o", got)
	}
}

func TestStoreKeepsPreexistingDirectoryMode(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	store := NewStore(dir)
	mustCreate(t, store, "bot")
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o750 {
		t.Fatalf("mode changed to %o", got)
	}
}

func TestStoreRejectsSymlinksAndBroadPermissions(t *testing.T) {
	t.Run("broad document", func(t *testing.T) {
		store := newTestStore(t)
		mustCreate(t, store, "bot")
		if err := os.Chmod(store.documentPath(), 0o640); err != nil {
			t.Fatal(err)
		}
		if _, err := store.List(); !errors.Is(err, ErrStoreInsecure) {
			t.Fatalf("list: %v", err)
		}
		if _, err := store.Create(context.Background(), "bot"); !errors.Is(err, ErrStoreInsecure) {
			t.Fatalf("create: %v", err)
		}
	})
	t.Run("symlink document", func(t *testing.T) {
		store := newTestStore(t)
		mustCreate(t, store, "bot")
		outside := filepath.Join(t.TempDir(), "outside.json")
		if err := os.Rename(store.documentPath(), outside); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, store.documentPath()); err != nil {
			t.Fatal(err)
		}
		if _, err := store.List(); !errors.Is(err, ErrStoreInsecure) {
			t.Fatalf("list: %v", err)
		}
	})
	t.Run("broad lock", func(t *testing.T) {
		store := newTestStore(t)
		mustCreate(t, store, "bot")
		if err := os.Chmod(store.lockPath(), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Create(context.Background(), "bot"); !errors.Is(err, ErrStoreInsecure) {
			t.Fatalf("create: %v", err)
		}
	})
	t.Run("symlink lock", func(t *testing.T) {
		store := newTestStore(t)
		if err := os.MkdirAll(store.dir, 0o700); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(t.TempDir(), "elsewhere")
		if err := os.Symlink(target, store.lockPath()); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Create(context.Background(), "bot"); !errors.Is(err, ErrStoreInsecure) {
			t.Fatalf("create: %v", err)
		}
		if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("the link must not be followed")
		}
	})
}
