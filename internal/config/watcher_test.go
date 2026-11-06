package config

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
)

// startLoop runs the real watch loop and stops it when the test ends.
func startLoop(t *testing.T, manager *Manager) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = manager.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

func newRealClockHarness(t *testing.T, text string, mutate ...func(*ManagerOptions)) *harness {
	t.Helper()
	return newHarness(t, text, append([]func(*ManagerOptions){func(o *ManagerOptions) { o.Clock = nil }}, mutate...)...)
}

func waitForGeneration(t *testing.T, manager *Manager, generation int64, limit time.Duration) time.Duration {
	t.Helper()
	start := time.Now()
	for manager.Capture().Generation() < generation {
		if time.Since(start) > limit {
			t.Fatalf("generation %d not active within %s; status %+v", generation, limit, manager.Status())
		}
		time.Sleep(10 * time.Millisecond)
	}
	return time.Since(start)
}

// atomicSave replaces the file the way editors do: a temporary file renamed over
// the target, which ends any watch placed on the file itself.
func atomicSave(t *testing.T, path, content string) {
	t.Helper()
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(temporary, path); err != nil {
		t.Fatal(err)
	}
}

func TestAtomicReplacementReloadWithinTwoSeconds(t *testing.T) {
	h := newRealClockHarness(t, baseYAML)
	startLoop(t, h.manager)
	time.Sleep(100 * time.Millisecond)

	atomicSave(t, h.path, replaced(t, baseYAML, "usd_per_day: 2.00", "usd_per_day: 5.00"))
	elapsed := waitForGeneration(t, h.manager, 2, 2*time.Second)
	t.Logf("first atomic save active after %s", elapsed)

	// A second replacement proves the watch survived the first one.
	atomicSave(t, h.path, replaced(t, baseYAML, "usd_per_day: 2.00", "usd_per_day: 7.00"))
	elapsed = waitForGeneration(t, h.manager, 3, 2*time.Second)
	t.Logf("second atomic save active after %s", elapsed)
	app, _ := h.manager.Capture().App("bot")
	if app.Budget.USDPerDay != 7_000_000 {
		t.Fatalf("budget: %d", app.Budget.USDPerDay)
	}
}

func TestInPlaceWriteReloadWithinTwoSeconds(t *testing.T) {
	h := newRealClockHarness(t, baseYAML)
	startLoop(t, h.manager)
	time.Sleep(100 * time.Millisecond)
	writeFile(t, h.path, replaced(t, baseYAML, "usd_per_day: 2.00", "usd_per_day: 5.00"))
	waitForGeneration(t, h.manager, 2, 2*time.Second)
}

func TestInvalidSaveKeepsRunningThenRecovers(t *testing.T) {
	h := newRealClockHarness(t, baseYAML)
	startLoop(t, h.manager)
	time.Sleep(100 * time.Millisecond)

	atomicSave(t, h.path, "providers: [")
	deadline := time.Now().Add(2 * time.Second)
	for h.manager.Status().Result != ResultRejected {
		if time.Now().After(deadline) {
			t.Fatalf("invalid save not reported: %+v", h.manager.Status())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if h.manager.Capture().Generation() != 1 {
		t.Fatal("the previous snapshot must stay active")
	}
	atomicSave(t, h.path, replaced(t, baseYAML, "usd_per_day: 2.00", "usd_per_day: 5.00"))
	waitForGeneration(t, h.manager, 2, 2*time.Second)
}

func TestSaveWithoutChangesRereadsSecretFile(t *testing.T) {
	text := replaced(t, baseYAML, "api_key: ${ANTHROPIC_API_KEY}", "api_key_file: secret.txt")
	dir := t.TempDir()
	path := filepath.Join(dir, "comporta.yaml")
	writeFile(t, path, text)
	writeFile(t, filepath.Join(dir, "secret.txt"), "first\n")
	manager, err := NewManager(ManagerOptions{
		Path: path, Options: Options{SystemTimezone: "UTC", Environment: NewEnvironment(nil)},
		inspect: func(string) (bool, error) { return false, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	startLoop(t, manager)
	time.Sleep(100 * time.Millisecond)

	writeFile(t, filepath.Join(dir, "secret.txt"), "second\n")
	time.Sleep(1500 * time.Millisecond)
	if manager.Capture().Generation() != 1 {
		t.Fatal("changing only the secret file must not reload by itself")
	}
	writeFile(t, path, text)
	waitForGeneration(t, manager, 2, 2*time.Second)
	if key, _ := manager.Capture().ProviderAPIKey("anthropic"); key.Reveal() != "second" {
		t.Fatal("saving the configuration must reread secret files")
	}
}

func TestReconciliationRecoversLostEvents(t *testing.T) {
	watcher := newFakeWatcher()
	h := newRealClockHarness(t, baseYAML, func(o *ManagerOptions) {
		o.newWatcher = func() (fsWatcher, error) { return watcher, nil }
	})
	startLoop(t, h.manager)
	writeFile(t, h.path, replaced(t, baseYAML, "usd_per_day: 2.00", "usd_per_day: 5.00"))
	// No event is ever delivered, so only reconciliation can find this change.
	waitForGeneration(t, h.manager, 2, 3*time.Second)
}

func TestDebounceAndContinuousEvents(t *testing.T) {
	h := newHarness(t, baseYAML)
	loop := newWatchLoop(h.manager)
	save := fsnotify.Event{Name: h.path, Op: fsnotify.Write}
	start := h.clock.Now()

	loop.onEvent(save)
	if !loop.pending || !loop.deadline().Equal(start.Add(500*time.Millisecond)) {
		t.Fatalf("trailing edge of 500 ms expected, got %s", loop.deadline().Sub(start))
	}
	h.clock.advance(400 * time.Millisecond)
	loop.onEvent(save)
	if !loop.deadline().Equal(start.Add(900 * time.Millisecond)) {
		t.Fatalf("another event must extend the wait, got %s", loop.deadline().Sub(start))
	}
	h.clock.advance(400 * time.Millisecond)
	loop.onEvent(save)
	if !loop.deadline().Equal(start.Add(time.Second)) {
		t.Fatalf("continuous events must be capped at 1 s, got %s", loop.deadline().Sub(start))
	}

	writeFile(t, h.path, replaced(t, baseYAML, "usd_per_day: 2.00", "usd_per_day: 5.00"))
	loop.onDeadline(context.Background())
	if loop.pending || h.manager.Capture().Generation() != 2 {
		t.Fatalf("the deadline must attempt once: pending %v, generation %d", loop.pending, h.manager.Capture().Generation())
	}

	loop.onEvent(fsnotify.Event{Name: filepath.Join(h.dir, "other.yaml"), Op: fsnotify.Write})
	loop.onEvent(fsnotify.Event{Name: h.path, Op: fsnotify.Chmod})
	if loop.pending {
		t.Fatal("other files and chmod must not schedule a reload")
	}
	loop.onEvent(fsnotify.Event{Name: h.path, Op: fsnotify.Rename})
	if !loop.pending {
		t.Fatal("a rename of the file must schedule a reload")
	}
}

func TestMissingFileWarnsEverySixtySeconds(t *testing.T) {
	h := newHarness(t, baseYAML)
	if err := os.Remove(h.path); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	warnings := func() int { return strings.Count(h.logs.String(), "config file missing") }

	h.manager.reconcile(ctx)
	if warnings() != 1 {
		t.Fatalf("the first absence warns immediately, got %d", warnings())
	}
	for range 59 {
		h.clock.advance(time.Second)
		if h.manager.reconcile(ctx) {
			t.Fatal("absence alone must not schedule a reload")
		}
	}
	if warnings() != 1 {
		t.Fatalf("warned again within 60 s: %d", warnings())
	}
	h.clock.advance(time.Second)
	h.manager.reconcile(ctx)
	if warnings() != 2 {
		t.Fatalf("expected a second warning at 60 s, got %d", warnings())
	}
	if len(h.sink.snapshot()) != 2 {
		t.Fatalf("notifications follow the same interval: %d", len(h.sink.snapshot()))
	}
	status := h.manager.Status()
	if status.Result != ResultRejected || !hasDiagnostic(status.Errors, CodeConfigMissing, "") {
		t.Fatalf("status: %+v", status)
	}
	if h.manager.Capture().Generation() != 1 {
		t.Fatal("the previous snapshot must stay active")
	}
}

func TestFileReturnRecovers(t *testing.T) {
	h := newRealClockHarness(t, baseYAML)
	startLoop(t, h.manager)
	time.Sleep(100 * time.Millisecond)
	if err := os.Remove(h.path); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for h.manager.Status().Result != ResultRejected {
		if time.Now().After(deadline) {
			t.Fatalf("absence not reported: %+v", h.manager.Status())
		}
		time.Sleep(10 * time.Millisecond)
	}
	atomicSave(t, h.path, replaced(t, baseYAML, "usd_per_day: 2.00", "usd_per_day: 5.00"))
	waitForGeneration(t, h.manager, 2, 3*time.Second)
	if h.manager.Status().Result != ResultApplied {
		t.Fatalf("status after recovery: %+v", h.manager.Status())
	}
}

func TestIdenticalFileReturnClearsFailure(t *testing.T) {
	h := newHarness(t, baseYAML)
	ctx := context.Background()
	loop := newWatchLoop(h.manager)
	if err := os.Remove(h.path); err != nil {
		t.Fatal(err)
	}
	loop.onTick(ctx)
	writeFile(t, h.path, baseYAML)
	if !h.manager.reconcile(ctx) {
		t.Fatal("the return of the file must schedule an attempt even with identical bytes")
	}
	if status := h.manager.Reload(ctx); status.Result != ResultUnchanged || len(status.Errors) != 0 {
		t.Fatalf("status: %+v", status)
	}
}

func TestWatcherOverflowReconciles(t *testing.T) {
	watcher := newFakeWatcher()
	h := newHarness(t, baseYAML, func(o *ManagerOptions) {
		o.newWatcher = func() (fsWatcher, error) { return watcher, nil }
	})
	loop := newWatchLoop(h.manager)
	loop.ensureWatching()
	if !loop.watching || watcher.adds != 1 {
		t.Fatalf("initial watch: %v %d", loop.watching, watcher.adds)
	}

	loop.onWatchError(fsnotify.ErrEventOverflow)
	if !loop.pending {
		t.Fatal("an overflow must force a new attempt even with an unchanged file")
	}
	if watcher.removes != 1 || watcher.adds != 2 || !loop.watching {
		t.Fatalf("an overflow must renew the watch: removes %d, adds %d", watcher.removes, watcher.adds)
	}
	if !strings.Contains(h.logs.String(), "event_overflow") {
		t.Fatalf("overflow not logged: %s", h.logs)
	}
	if h.manager.Capture().Generation() != 1 {
		t.Fatal("the active configuration must stay untouched")
	}
	loop.onDeadline(context.Background())
	if h.manager.Status().Result != ResultUnchanged {
		t.Fatalf("status: %+v", h.manager.Status())
	}
}

func TestDirectoryRemovalRenewsWatch(t *testing.T) {
	watcher := newFakeWatcher()
	h := newHarness(t, baseYAML, func(o *ManagerOptions) {
		o.newWatcher = func() (fsWatcher, error) { return watcher, nil }
	})
	loop := newWatchLoop(h.manager)
	loop.ensureWatching()
	loop.onEvent(fsnotify.Event{Name: h.dir, Op: fsnotify.Remove})
	if loop.watching {
		t.Fatal("removal of the directory ends the watch")
	}
	loop.onTick(context.Background())
	if !loop.watching || watcher.adds != 2 {
		t.Fatalf("the next tick must watch again: %v %d", loop.watching, watcher.adds)
	}
}
