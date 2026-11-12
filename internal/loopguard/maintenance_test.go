package loopguard

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/config"
)

func TestExpiredSignaturesRemoved(t *testing.T) {
	e := newEnv(t, Options{})
	u := user("agent", 1)
	for i := range 5 {
		e.allow(u, fmt.Sprint(i), defaultPolicy())
	}
	if e.manager.Occurrences() != 10 {
		t.Fatalf("occurrences: %d", e.manager.Occurrences())
	}
	e.clock.Advance(30 * time.Second)
	e.manager.mu.Lock()
	e.manager.sweepLocked(e.clock.Elapsed())
	state := e.manager.subjects[u]
	e.manager.mu.Unlock()
	if state == nil || state.sigs.len() != 0 || state.sigs.counts != nil || e.manager.Occurrences() != 5 {
		t.Fatalf("signatures past the window must be gone: %d", e.manager.Occurrences())
	}
	e.clock.Advance(30 * time.Second)
	e.manager.mu.Lock()
	e.manager.sweepLocked(e.clock.Elapsed())
	e.manager.mu.Unlock()
	if e.manager.Subjects() != 0 || e.manager.Occurrences() != 0 {
		t.Fatal("idle subject kept")
	}
}

func TestCooldownDropsSignatures(t *testing.T) {
	e := newEnv(t, Options{})
	u := user("agent", 1)
	for range 10 {
		e.allow(u, "same", defaultPolicy())
	}
	e.cut(u, "same", defaultPolicy())
	e.manager.mu.Lock()
	state := e.manager.subjects[u]
	e.manager.mu.Unlock()
	if state.sigs.len() != 0 || state.volume.len() != 0 || state.held != 0 || e.manager.Occurrences() != 0 {
		t.Fatal("a cut keeps digests during the cooldown")
	}
}

func TestCapacityRejectsWithoutEviction(t *testing.T) {
	e := newEnv(t, Options{MaxSubjects: 2, MaxOccurrences: 5})
	p := defaultPolicy()
	p.SimilarRequests = 1
	e.allow(user("a", 1), "x", p)
	e.cut(user("a", 1), "x", p)
	e.allow(user("a", 2), "y", p)
	if _, err := e.manager.Check(e.request(user("a", 3), "z", p)); err != ErrCapacity {
		t.Fatalf("third subject: %v", err)
	}
	// The protected subject still answers with its cooldown.
	if d := e.cut(user("a", 1), "x", p); d.First {
		t.Fatal("cooldown lost under pressure")
	}
	e.allow(user("a", 2), "w", p)
	if _, err := e.manager.Check(e.request(user("a", 2), "v", p)); err != ErrCapacity {
		t.Fatalf("occurrence ceiling: %v", err)
	}
	if e.manager.Occurrences() != 4 {
		t.Fatalf("partial insertion: %d", e.manager.Occurrences())
	}
	e.clock.Advance(RateWindow)
	e.allow(user("a", 3), "z", p)
}

func TestCleanupCannotSplitSubjectLock(t *testing.T) {
	e := newEnv(t, Options{})
	u := user("agent", 1)
	p := defaultPolicy()
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			e.clock.Advance(time.Second)
			e.manager.mu.Lock()
			e.manager.sweepLocked(e.clock.Elapsed())
			e.manager.mu.Unlock()
		}
	})
	for range 2000 {
		_, _ = e.manager.Check(e.request(u, "", p))
	}
	close(stop)
	wg.Wait()
	e.clock.Advance(time.Hour)
	e.manager.mu.Lock()
	e.manager.sweepLocked(e.clock.Elapsed())
	e.manager.mu.Unlock()
	if e.manager.Occurrences() != 0 {
		t.Fatalf("occurrence accounting drifted: %d", e.manager.Occurrences())
	}
}

func TestReloadChangedThresholdResetsHistory(t *testing.T) {
	e := newEnv(t, Options{})
	u := user("agent", 1)
	for range 9 {
		e.allow(u, "same", defaultPolicy())
	}
	p := defaultPolicy()
	p.Generation, p.SimilarRequests = 2, 11
	for range 11 {
		e.allow(u, "same", p)
	}
	e.cut(u, "same", p)
	// A change unrelated to detection keeps the history.
	v := user("agent", 2)
	for range 10 {
		e.allow(v, "same", p)
	}
	q := p
	q.Generation, q.Cooldown = 3, time.Minute
	e.allow(v, "same", q)
	if d := e.cut(v, "same", q); d.Episode.Cooldown != time.Minute {
		t.Fatalf("new episode takes the new cooldown: %+v", d)
	}
}

func TestReloadPreservesCooldown(t *testing.T) {
	e := newEnv(t, Options{})
	u := user("agent", 1)
	for range 10 {
		e.allow(u, "same", defaultPolicy())
	}
	first := e.cut(u, "same", defaultPolicy())
	p := defaultPolicy()
	p.Generation, p.SimilarRequests, p.Cooldown = 2, 50, time.Second
	d := e.cut(u, "same", p)
	if d.Episode != first.Episode {
		t.Fatal("reload changed an active episode")
	}
}

func TestDisableClearsCooldown(t *testing.T) {
	e := newEnv(t, Options{})
	u := user("agent", 1)
	for range 10 {
		e.allow(u, "same", defaultPolicy())
	}
	e.cut(u, "same", defaultPolicy())
	off := defaultPolicy()
	off.Generation, off.Enabled = 2, false
	e.allow(u, "same", off)
	on := defaultPolicy()
	on.Generation = 3
	for range 10 {
		e.allow(u, "same", on)
	}
	e.cut(u, "same", on)
}

func TestStaleGenerationCannotRestorePolicy(t *testing.T) {
	e := newEnv(t, Options{})
	u := user("agent", 1)
	off := defaultPolicy()
	off.Generation, off.Enabled = 2, false
	e.allow(u, "same", off)
	if _, err := e.manager.Check(e.request(u, "same", defaultPolicy())); err != ErrStale {
		t.Fatalf("older generation: %v", err)
	}
	if e.manager.Subjects() != 0 {
		t.Fatal("stale policy recreated state")
	}
}

type staticSource struct {
	mu       sync.Mutex
	snapshot *config.Snapshot
}

func (s *staticSource) Capture() *config.Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshot
}

const baseConfig = `timezone: UTC
providers:
  anthropic:
    api_format: anthropic
    base_url: http://127.0.0.1:9
    models: [m]
apps:
%s`

func rewrite(t *testing.T, manager *config.Manager, path, apps string) {
	t.Helper()
	if err := os.WriteFile(path, fmt.Appendf(nil, baseConfig, apps), 0o600); err != nil {
		t.Fatal(err)
	}
	if status := manager.Reload(context.Background()); status.Result != config.ResultApplied {
		t.Fatalf("reload: %+v", status)
	}
}

func TestRemovedAppCleared(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "comporta.yaml")
	apps := "  agent:\n    allowed_models: [m]\n  other:\n    allowed_models: [m]\n"
	if err := os.WriteFile(path, fmt.Appendf(nil, baseConfig, apps), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.NewManager(config.ManagerOptions{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, Options{Source: cfg})
	policy, _ := PolicyFor(cfg.Capture(), "agent")
	e.allow(user("agent", 1), "x", policy)
	e.allow(user("other", 1), "x", policy)
	rewrite(t, cfg, path, "  other:\n    allowed_models: [m]\n")
	e.manager.Reconcile()
	if apps := e.manager.Apps(); len(apps) != 1 || apps[0] != "other" {
		t.Fatalf("removed app kept: %v", apps)
	}
	if _, err := e.manager.Check(e.request(user("agent", 1), "x", policy)); err != ErrStale {
		t.Fatalf("old revision recreated a removed app: %v", err)
	}
	rewrite(t, cfg, path, "  other:\n    allowed_models: [m]\n    loop_guard:\n      enabled: false\n")
	e.manager.Reconcile()
	if e.manager.Subjects() != 0 {
		t.Fatal("disabled app kept without traffic")
	}
}

func TestWorkerStops(t *testing.T) {
	e := newEnv(t, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.manager.Run(ctx) }()
	u := user("agent", 1)
	e.allow(u, "x", defaultPolicy())
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not stop")
	}
	if e.manager.Subjects() != 0 || e.manager.Occurrences() != 0 {
		t.Fatal("state kept after stop")
	}
}

func TestWorkerSweepsOnDeadline(t *testing.T) {
	e := newEnv(t, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = e.manager.Run(ctx) }()
	e.allow(user("agent", 1), "x", defaultPolicy())
	deadline := time.Now().Add(5 * time.Second)
	for e.manager.Subjects() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("worker never released the expired subject")
		}
		e.clock.Advance(time.Second)
		time.Sleep(time.Millisecond)
	}
}
