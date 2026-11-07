package useridentity

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	helperDirEnv = "COMPORTA_TEST_HMAC_DIR"
	sentinel     = "comporta-test-pseudonym:"
)

// writePrivate creates a file through the same private path as production, because on
// Windows a plain write inherits an ACL that the key check refuses.
func writePrivate(t *testing.T, path string, data []byte) {
	t.Helper()
	file, err := createPrivateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func readKey(t *testing.T, dir string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, keyFile))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestKeyGeneratedExactlyOnce(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "data")
	var logs strings.Builder
	first, err := Open(context.Background(), Options{Dir: dir, Logger: slog.New(slog.NewJSONHandler(&logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	created := readKey(t, dir)
	if len(created) != keyBytes {
		t.Fatalf("key has %d bytes", len(created))
	}
	if strings.Contains(logs.String(), fmt.Sprintf("%x", created)) {
		t.Fatal("the key reached the logs")
	}
	second := mustOpen(t, dir)
	if !bytes.Equal(created, readKey(t, dir)) {
		t.Fatal("the key was rewritten")
	}
	a, _ := first.LookupHash("bot", "user_123")
	b, _ := second.LookupHash("bot", "user_123")
	if a != b {
		t.Fatal("the loaded key differs from the generated one")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".tmp") {
			t.Fatalf("temporary file left behind: %s", entry.Name())
		}
	}
}

func TestDistinctDirectoriesGetDistinctKeys(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	mustOpen(t, first)
	mustOpen(t, second)
	if bytes.Equal(readKey(t, first), readKey(t, second)) {
		t.Fatal("keys must be random")
	}
}

// TestHelperOpenKey is not a test by itself: it is the body of the real subprocesses of
// TestConcurrentProcessesShareKey.
func TestHelperOpenKey(t *testing.T) {
	dir := os.Getenv(helperDirEnv)
	if dir == "" {
		t.Skip("runs only as a subprocess")
	}
	service, err := Open(context.Background(), Options{Dir: dir, LockTimeout: 30 * time.Second})
	if err != nil {
		fmt.Println("failed:", err)
		os.Exit(1)
	}
	hash, err := service.LookupHash("bot", "user_123")
	if err != nil {
		fmt.Println("failed:", err)
		os.Exit(1)
	}
	fmt.Println(sentinel + hash.Hex())
}

func TestConcurrentProcessesShareKey(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	const processes = 8
	outputs := make([][]byte, processes)
	errs := make([]error, processes)
	var wg sync.WaitGroup
	for i := range processes {
		wg.Go(func() {
			command := exec.Command(os.Args[0], "-test.run=^TestHelperOpenKey$", "-test.v")
			command.Env = append(os.Environ(), helperDirEnv+"="+dir)
			outputs[i], errs[i] = command.CombinedOutput()
		})
	}
	wg.Wait()
	seen := map[string]bool{}
	for i := range processes {
		if errs[i] != nil {
			t.Fatalf("process %d: %v\n%s", i, errs[i], outputs[i])
		}
		_, after, found := strings.Cut(string(outputs[i]), sentinel)
		if !found {
			t.Fatalf("process %d printed no pseudonym:\n%s", i, outputs[i])
		}
		seen[strings.TrimSpace(strings.SplitN(after, "\n", 2)[0])] = true
	}
	if len(seen) != 1 {
		t.Fatalf("processes disagree on the key: %v", seen)
	}
	if got := readKey(t, dir); len(got) != keyBytes {
		t.Fatalf("key has %d bytes", len(got))
	}
}

func TestInvalidKeySizeNeverRegenerated(t *testing.T) {
	cases := map[string][]byte{
		"empty":   {},
		"short":   bytes.Repeat([]byte{1}, 31),
		"long":    bytes.Repeat([]byte{1}, 33),
		"textual": []byte(strings.Repeat("ab", 32)),
		"newline": append(bytes.Repeat([]byte{1}, 32), '\n'),
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writePrivate(t, filepath.Join(dir, keyFile), content)
			for _, reset := range []bool{false, true} {
				_, err := Open(context.Background(), Options{Dir: dir, ResetHistory: reset})
				if !errors.Is(err, ErrKeySize) {
					t.Fatalf("reset %v: got %v", reset, err)
				}
			}
			if !bytes.Equal(readKey(t, dir), content) {
				t.Fatal("an invalid key was replaced")
			}
		})
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("entropy unavailable") }

func TestEntropyFailureStartsNoService(t *testing.T) {
	dir := t.TempDir()
	if _, err := Open(context.Background(), Options{Dir: dir, Entropy: failingReader{}}); !errors.Is(err, ErrKeyUnavailable) {
		t.Fatalf("got %v", err)
	}
	if _, err := Open(context.Background(), Options{Dir: dir, Entropy: bytes.NewReader(make([]byte, keyBytes))}); !errors.Is(err, ErrKeyUnavailable) {
		t.Fatalf("a short source: got %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, keyFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("no key may exist after an entropy failure")
	}
}

func TestPartialWriteAndPublishFailure(t *testing.T) {
	failure := errors.New("disk failure")
	cases := map[string]func(*fileOps){
		"create": func(o *fileOps) { o.create = func(string) (*os.File, error) { return nil, failure } },
		"write": func(o *fileOps) {
			o.write = func(file *os.File, data []byte) error {
				_, _ = file.Write(data[:5])
				return failure
			}
		},
		"sync":    func(o *fileOps) { o.sync = func(*os.File) error { return failure } },
		"publish": func(o *fileOps) { o.publish = func(string, string) error { return failure } },
	}
	for name, breakIt := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			ops := defaultFileOps()
			breakIt(&ops)
			if _, err := open(context.Background(), Options{Dir: dir}, ops); !errors.Is(err, ErrKeyUnavailable) {
				t.Fatalf("got %v", err)
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if entry.Name() != lockFile {
					t.Errorf("left behind: %s", entry.Name())
				}
			}
			mustOpen(t, dir)
		})
	}
	t.Run("durability failure keeps the published key", func(t *testing.T) {
		dir := t.TempDir()
		ops := defaultFileOps()
		ops.syncDir = func(string) error { return failure }
		if _, err := open(context.Background(), Options{Dir: dir}, ops); !errors.Is(err, ErrKeyUnavailable) {
			t.Fatalf("got %v", err)
		}
		published := readKey(t, dir)
		mustOpen(t, dir)
		if !bytes.Equal(published, readKey(t, dir)) {
			t.Fatal("the published key was replaced")
		}
	})
}

func TestAbandonedTemporaryIsCleared(t *testing.T) {
	dir := t.TempDir()
	abandoned := filepath.Join(dir, ".hmac-abandoned.tmp")
	writePrivate(t, abandoned, []byte("never published"))
	mustOpen(t, dir)
	if _, err := os.Lstat(abandoned); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("what a dead process left behind must be removed")
	}
}

func TestPublicationLostToAnotherProcessLoadsItsKey(t *testing.T) {
	dir := t.TempDir()
	winner := bytes.Repeat([]byte{7}, keyBytes)
	ops := defaultFileOps()
	ops.publish = func(from, to string) error {
		writePrivate(t, to, winner)
		return publishNoReplace(from, to)
	}
	service, err := open(context.Background(), Options{Dir: dir}, ops)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readKey(t, dir), winner) {
		t.Fatal("the key of the other process was replaced")
	}
	got, _ := service.LookupHash("bot", "user_123")
	want, _ := newService(winner).LookupHash("bot", "user_123")
	if got != want {
		t.Fatal("the service must use the winning key")
	}
}

func TestPublicationDoesNotReplaceExistingKey(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, keyFile)
	original := bytes.Repeat([]byte{9}, keyBytes)
	writePrivate(t, existing, original)
	temporary := filepath.Join(dir, ".hmac-candidate.tmp")
	writePrivate(t, temporary, bytes.Repeat([]byte{1}, keyBytes))
	if err := publishNoReplace(temporary, existing); !errors.Is(err, errAlreadyExists) {
		t.Fatalf("got %v", err)
	}
	if !bytes.Equal(readKey(t, dir), original) {
		t.Fatal("publication replaced an existing key")
	}
}

func TestMissingKeyWithUncertainHistoryFailsClosed(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "usage.db"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), Options{Dir: dir}); !errors.Is(err, ErrHistoryExists) {
		t.Fatalf("got %v", err)
	}
	if err := fmt.Sprint(ErrHistoryExists); err != "hmac.key not found but usage data exists; restore the key or start with --reset-user-history" {
		t.Fatalf("documented message changed: %s", err)
	}
	if _, err := os.Stat(filepath.Join(dir, keyFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("no key may be generated")
	}
}

func TestLockTimeoutAndCancellation(t *testing.T) {
	dir := t.TempDir()
	held, err := openLockFile(filepath.Join(dir, lockFile))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	if locked, err := tryLock(held); err != nil || !locked {
		t.Fatalf("locked %v, %v", locked, err)
	}
	began := time.Now()
	if _, err := Open(context.Background(), Options{Dir: dir, LockTimeout: 100 * time.Millisecond}); !errors.Is(err, ErrKeyUnavailable) {
		t.Fatalf("timeout: %v", err)
	}
	if elapsed := time.Since(began); elapsed > 3*time.Second {
		t.Fatalf("waited %s", elapsed)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Open(ctx, Options{Dir: dir}); !errors.Is(err, ErrKeyUnavailable) {
		t.Fatalf("cancel: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, keyFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("no key may be generated without the lock")
	}
}

func TestStartupMessagesCarryNoDetail(t *testing.T) {
	dir := t.TempDir()
	var captured strings.Builder
	_, err := Open(context.Background(), Options{Dir: dir, Entropy: failingReader{}, Logger: slog.New(slog.NewTextHandler(io.MultiWriter(&captured), nil))})
	if err == nil || strings.Contains(err.Error(), dir) || strings.Contains(err.Error(), "entropy unavailable") {
		t.Fatalf("got %v", err)
	}
	if strings.Contains(captured.String(), dir) {
		t.Fatalf("logs: %s", captured.String())
	}
}
