//go:build !windows

package useridentity

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestKeyModeAndHandleValidation(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	mustOpen(t, dir)
	for name, want := range map[string]os.FileMode{keyFile: 0o600, lockFile: 0o600} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != want {
			t.Errorf("%s has mode %o", name, info.Mode().Perm())
		}
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Errorf("a new directory has mode %o", info.Mode().Perm())
	}

	if err := os.Chmod(filepath.Join(dir, keyFile), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), Options{Dir: dir}); !errors.Is(err, ErrKeyPermissions) {
		t.Fatalf("a broad mode: %v", err)
	}
}

func TestKeySymlinkAndFIFORejected(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(t.TempDir(), "elsewhere")
		if err := os.WriteFile(target, bytes.Repeat([]byte{1}, keyBytes), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(dir, keyFile)); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(context.Background(), Options{Dir: dir}); !errors.Is(err, ErrKeyPermissions) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("fifo", func(t *testing.T) {
		dir := t.TempDir()
		if err := unix.Mkfifo(filepath.Join(dir, keyFile), 0o600); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			_, err := Open(context.Background(), Options{Dir: dir})
			done <- err
		}()
		select {
		case err := <-done:
			if !errors.Is(err, ErrKeyPermissions) {
				t.Fatalf("got %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a FIFO hung the startup")
		}
	})
	t.Run("symlinked lock", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Symlink(filepath.Join(t.TempDir(), "elsewhere"), filepath.Join(dir, lockFile)); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(context.Background(), Options{Dir: dir}); !errors.Is(err, ErrKeyPermissions) {
			t.Fatalf("got %v", err)
		}
	})
}
