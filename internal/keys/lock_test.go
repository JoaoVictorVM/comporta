package keys

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	helperModeEnv = "COMPORTA_KEYS_TEST_HELPER"
	helperDirEnv  = "COMPORTA_KEYS_TEST_DIR"
)

// TestMain lets the test binary act as a separate process using the same store,
// since the lock only has meaning between real processes.
func TestMain(m *testing.M) {
	switch os.Getenv(helperModeEnv) {
	case "create":
		store := NewStore(os.Getenv(helperDirEnv))
		store.lockTimeout = 60 * time.Second
		if _, err := store.Create(context.Background(), "bot"); err != nil {
			fmt.Println(err)
			os.Exit(3)
		}
		os.Exit(0)
	case "hold":
		store := NewStore(os.Getenv(helperDirEnv))
		if _, err := store.lock(context.Background()); err != nil {
			fmt.Println(err)
			os.Exit(3)
		}
		fmt.Println("locked")
		// Holds the lock until killed; only process death may release it.
		_, _ = io.Copy(io.Discard, os.Stdin)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func helperCommand(t *testing.T, mode, dir string) *exec.Cmd {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable)
	command.Env = append(os.Environ(), helperModeEnv+"="+mode, helperDirEnv+"="+dir)
	return command
}

func TestConcurrentProcessesCannotCreateThirdKey(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	const processes = 6
	results := make([]error, processes)
	output := make([]string, processes)
	var group sync.WaitGroup
	for i := range processes {
		group.Go(func() {
			data, err := helperCommand(t, "create", dir).CombinedOutput()
			results[i], output[i] = err, string(data)
		})
	}
	group.Wait()

	succeeded := 0
	for i, err := range results {
		if err == nil {
			succeeded++
			continue
		}
		if !strings.Contains(output[i], "already has 2 active keys") {
			t.Fatalf("unexpected failure: %v: %s", err, output[i])
		}
	}
	if succeeded != MaxActiveKeysPerApp {
		t.Fatalf("%d processes succeeded", succeeded)
	}
	entries, err := NewStore(dir).List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != MaxActiveKeysPerApp {
		t.Fatalf("the document holds %d keys", len(entries))
	}
}

func TestProcessExitReleasesLock(t *testing.T) {
	store := newTestStore(t)
	if err := ensurePrivateDir(store.dir); err != nil {
		t.Fatal(err)
	}
	holder := helperCommand(t, "hold", store.dir)
	stdin, err := holder.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stdin.Close() }()
	stdout, err := holder.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || !strings.Contains(line, "locked") {
		_ = holder.Process.Kill()
		t.Fatalf("helper did not lock: %q %v", line, err)
	}

	store.lockTimeout = 150 * time.Millisecond
	if _, err := store.lock(context.Background()); !errors.Is(err, ErrLockTimeout) {
		_ = holder.Process.Kill()
		t.Fatalf("another process holds the lock: %v", err)
	}
	if err := holder.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = holder.Wait()

	store.lockTimeout = 10 * time.Second
	unlock, err := store.lock(context.Background())
	if err != nil {
		t.Fatalf("process death must release the lock: %v", err)
	}
	unlock()
	if _, err := os.Stat(store.lockPath()); err != nil {
		t.Fatalf("the lock file must remain: %v", err)
	}
}

func TestLockTimeoutAndCancellation(t *testing.T) {
	store := newTestStore(t)
	if err := ensurePrivateDir(store.dir); err != nil {
		t.Fatal(err)
	}
	unlock, err := store.lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	store.lockTimeout = 100 * time.Millisecond
	start := time.Now()
	if _, err := store.lock(context.Background()); !errors.Is(err, ErrLockTimeout) {
		t.Fatalf("err = %v", err)
	}
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond || elapsed > 3*time.Second {
		t.Fatalf("timeout took %v", elapsed)
	}

	store.lockTimeout = time.Minute
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	start = time.Now()
	if _, err := store.lock(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("cancellation must interrupt the wait")
	}
}

func TestDefaultLockTimeoutIsFiveSeconds(t *testing.T) {
	if got := NewStore(t.TempDir()).lockTimeout; got != 5*time.Second {
		t.Fatalf("timeout %v", got)
	}
}
