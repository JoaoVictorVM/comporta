package config

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 11, 6, 3, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type recordingSink struct {
	mu     sync.Mutex
	events []Event
	err    error
	panics bool
}

func (s *recordingSink) Publish(_ context.Context, event Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, event)
	if s.panics {
		panic("sink failure")
	}
	return s.err
}

func (s *recordingSink) snapshot() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Event(nil), s.events...)
}

// safeBuffer lets the logger write from the watch loop while a test reads.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type harness struct {
	manager *Manager
	path    string
	dir     string
	clock   *fakeClock
	sink    *recordingSink
	logs    *safeBuffer
}

const testAPIKey = "canary-test-api-key"

func newHarness(t *testing.T, text string, mutate ...func(*ManagerOptions)) *harness {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "comporta.yaml")
	writeFile(t, path, text)
	h := &harness{path: path, dir: dir, clock: newFakeClock(), sink: &recordingSink{}, logs: &safeBuffer{}}
	options := ManagerOptions{
		Path: path,
		Options: Options{
			SystemTimezone: "UTC",
			Environment:    NewEnvironment(map[string]string{"ANTHROPIC_API_KEY": testAPIKey}),
		},
		Logger: slog.New(slog.NewJSONHandler(h.logs, nil)),
		Clock:  h.clock,
		Sink:   h.sink,
		inspect: func(string) (bool, error) {
			return false, nil
		},
	}
	for _, change := range mutate {
		change(&options)
	}
	manager, err := NewManager(options)
	if err != nil {
		t.Fatalf("initial load: %v", err)
	}
	h.manager = manager
	return h
}

func (h *harness) save(t *testing.T, text string) ReloadStatus {
	t.Helper()
	writeFile(t, h.path, text)
	h.clock.advance(time.Second)
	return h.manager.Reload(context.Background())
}

func TestInitialStatus(t *testing.T) {
	h := newHarness(t, baseYAML)
	status := h.manager.Status()
	if status.Result != ResultInitial || status.ActiveGeneration != 1 || len(status.Errors) != 0 {
		t.Fatalf("status: %+v", status)
	}
	if h.manager.Capture().Generation() != 1 {
		t.Fatal("generations start at 1")
	}
	if len(h.sink.snapshot()) != 0 {
		t.Fatal("the initial load is not a change")
	}
}

func TestStartupHasNoFallback(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "comporta.yaml")
	if _, err := NewManager(ManagerOptions{Path: path, Options: Options{SystemTimezone: "UTC", Environment: NewEnvironment(nil)}}); err == nil {
		t.Fatal("a missing file must abort startup")
	}
	writeFile(t, path, replaced(t, baseYAML, "${ANTHROPIC_API_KEY}", literalCanary))
	_, err := NewManager(ManagerOptions{Path: path, Options: Options{SystemTimezone: "UTC", Environment: NewEnvironment(nil)}})
	if err == nil || !strings.Contains(err.Error(), "providers.anthropic.api_key contains a literal secret") {
		t.Fatalf("a literal key must abort startup: %v", err)
	}
	if strings.Contains(err.Error(), literalCanary) {
		t.Fatal("the error exposes the literal")
	}
	writeFile(t, path, baseYAML)
	_, err = NewManager(ManagerOptions{Path: path, Options: Options{SystemTimezone: "UTC", Environment: NewEnvironment(nil)}})
	if err == nil || !strings.Contains(err.Error(), "environment variable ANTHROPIC_API_KEY is not set") {
		t.Fatalf("a missing variable must abort startup: %v", err)
	}
}

func TestValidReloadPublishesAndReportsChange(t *testing.T) {
	h := newHarness(t, baseYAML)
	status := h.save(t, replaced(t, baseYAML, "usd_per_day: 2.00", "usd_per_day: 5.00"))
	if status.Result != ResultApplied || status.ActiveGeneration != 2 {
		t.Fatalf("status: %+v", status)
	}
	app, _ := h.manager.Capture().App("bot")
	if app.Budget.USDPerDay != 5_000_000 {
		t.Fatalf("budget was not applied: %d", app.Budget.USDPerDay)
	}
	events := h.sink.snapshot()
	if len(events) != 1 || events[0].Type != EventConfigChanged || events[0].Generation != 2 {
		t.Fatalf("events: %+v", events)
	}
	change := events[0].Changes[0]
	if change.Path != "apps.bot.budget.usd_per_day" || *change.Old != "2.000000" || *change.New != "5.000000" {
		t.Fatalf("change: %+v", change)
	}
	const documented = "config reloaded: 1 app, 1 change (bot.budget.usd_per_day 2.00 → 5.00)"
	if !strings.Contains(h.logs.String(), documented) {
		t.Fatalf("log lacks the documented text: %s", h.logs)
	}
}

func TestInvalidReloadKeepsPrevious(t *testing.T) {
	h := newHarness(t, baseYAML)
	before := h.manager.Capture()
	status := h.save(t, replaced(t, baseYAML, "  bot:\n", "  bot:\n    on_limit: sometimes\n"))
	if status.Result != ResultRejected || status.ActiveGeneration != 1 {
		t.Fatalf("status: %+v", status)
	}
	if h.manager.Capture() != before {
		t.Fatal("a rejected reload must keep the same snapshot")
	}
	if len(status.Errors) != 1 || status.Errors[0].Code != CodeInvalidPolicy || status.Errors[0].Line == 0 {
		t.Fatalf("errors must reach the status with a position: %+v", status.Errors)
	}
	if status.LastAttemptAt.Equal(status.LastSuccessAt) {
		t.Fatal("the attempt time must advance without the success time")
	}
	const documented = "config reload rejected: apps.bot.on_limit: must be one of block, downgrade, warn (previous configuration still active)"
	if !strings.Contains(h.logs.String(), documented) {
		t.Fatalf("log lacks the documented text: %s", h.logs)
	}
	events := h.sink.snapshot()
	if len(events) != 1 || events[0].Type != EventConfigReloadFailed || events[0].Generation != 1 {
		t.Fatalf("events: %+v", events)
	}

	status = h.save(t, replaced(t, baseYAML, "usd_per_day: 2.00", "usd_per_day: 3.00"))
	if status.Result != ResultApplied || len(status.Errors) != 0 || status.ActiveGeneration != 2 {
		t.Fatalf("a valid save must recover: %+v", status)
	}
}

func TestStaticChangeRejectsEntireCandidate(t *testing.T) {
	cases := map[string]string{
		"listeners.proxy.port": "listeners: {proxy: {port: 9000}}\n",
		"listeners.proxy.bind": "listeners: {proxy: {bind: 127.0.0.2}}\n",
		"listeners.admin.port": "listeners: {admin: {port: 9001}}\n",
		"listeners.admin.bind": "listeners: {admin: {bind: 127.0.0.2}}\n",
		"data_dir":             "data_dir: ./elsewhere\n",
	}
	for path, line := range cases {
		t.Run(path, func(t *testing.T) {
			h := newHarness(t, baseYAML)
			edit := replaced(t, baseYAML, "usd_per_day: 2.00", "usd_per_day: 9.00") + line
			status := h.save(t, edit)
			if status.Result != ResultRejected || !hasDiagnostic(status.Errors, CodeRestartRequired, path) {
				t.Fatalf("status: %+v", status)
			}
			app, _ := h.manager.Capture().App("bot")
			if app.Budget.USDPerDay != 2_000_000 {
				t.Fatal("dynamic fields of a rejected edit must not apply")
			}
		})
	}
	h := newHarness(t, baseYAML)
	h.save(t, baseYAML+"listeners: {proxy: {port: 9000}}\n")
	const documented = "config reload rejected: listeners.proxy.port: change requires restart (previous configuration still active)"
	if !strings.Contains(h.logs.String(), documented) {
		t.Fatalf("log lacks the documented text: %s", h.logs)
	}
}

func TestNormalizedStaticNoChange(t *testing.T) {
	text := baseYAML + "data_dir: ./data\nlisteners: {proxy: {bind: 127.0.0.1, port: 8080}, admin: {bind: \"::ffff:127.0.0.1\"}}\n"
	h := newHarness(t, baseYAML)
	status := h.save(t, text)
	if status.Result != ResultUnchanged {
		t.Fatalf("equivalent spellings are not a change: %+v", status)
	}
	status = h.save(t, replaced(t, text, "data_dir: ./data", "data_dir: ./sub/../data"))
	if status.Result != ResultUnchanged {
		t.Fatalf("an equivalent directory is not a change: %+v", status)
	}
}

func TestSecretOnlyChangePublishesGeneration(t *testing.T) {
	secretPath := "secret.txt"
	text := replaced(t, baseYAML, "api_key: ${ANTHROPIC_API_KEY}", "api_key_file: "+secretPath)
	dir := t.TempDir()
	path := filepath.Join(dir, "comporta.yaml")
	writeFile(t, path, text)
	writeFile(t, filepath.Join(dir, secretPath), "first-canary\n")
	sink := &recordingSink{}
	logs := &safeBuffer{}
	manager, err := NewManager(ManagerOptions{
		Path: path, Options: Options{SystemTimezone: "UTC", Environment: NewEnvironment(nil)},
		Clock: newFakeClock(), Sink: sink, Logger: slog.New(slog.NewJSONHandler(logs, nil)),
		inspect: func(string) (bool, error) { return false, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	publicBefore := manager.Capture().EffectiveYAML()

	writeFile(t, filepath.Join(dir, secretPath), "second-canary\n")
	status := manager.Reload(context.Background())
	if status.Result != ResultApplied || status.ActiveGeneration != 2 {
		t.Fatalf("status: %+v", status)
	}
	if manager.Capture().EffectiveYAML() != publicBefore {
		t.Fatal("the public projection of a secret change stays identical")
	}
	key, _ := manager.Capture().ProviderAPIKey("anthropic")
	if key.Reveal() != "second-canary" {
		t.Fatalf("new secret was not loaded")
	}
	events := sink.snapshot()
	if len(events) != 1 || len(events[0].Changes) != 1 {
		t.Fatalf("events: %+v", events)
	}
	change := events[0].Changes[0]
	if change.Path != "providers.anthropic.api_key" || !change.SecretChanged || *change.Old != Redacted || *change.New != Redacted {
		t.Fatalf("change: %+v", change)
	}
	if strings.Contains(logs.String(), "canary") {
		t.Fatalf("secret leaked to logs: %s", logs)
	}
}

func TestNoopReloadDoesNotEmitChange(t *testing.T) {
	h := newHarness(t, baseYAML)
	before := h.manager.Capture()
	status := h.save(t, baseYAML+"# only a comment changed\n")
	if status.Result != ResultUnchanged || status.ActiveGeneration != 1 {
		t.Fatalf("status: %+v", status)
	}
	if h.manager.Capture() != before {
		t.Fatal("a no-op must keep the same snapshot")
	}
	if len(h.sink.snapshot()) != 0 {
		t.Fatal("a no-op must not emit an event")
	}
	if status.LastSuccessAt.Equal(before.LoadedAt()) {
		t.Fatal("a no-op still records a successful attempt")
	}
}

func TestSinkFailureDoesNotAffectPublication(t *testing.T) {
	for name, sink := range map[string]*recordingSink{
		"error": {err: errors.New("canary-sink-error")},
		"panic": {panics: true},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, baseYAML, func(o *ManagerOptions) { o.Sink = sink })
			status := h.save(t, replaced(t, baseYAML, "usd_per_day: 2.00", "usd_per_day: 5.00"))
			if status.Result != ResultApplied || h.manager.Capture().Generation() != 2 {
				t.Fatalf("publication must survive a sink failure: %+v", status)
			}
			if strings.Contains(h.logs.String(), "canary-sink-error") {
				t.Fatal("sink error text must not be logged")
			}
		})
	}
}

func TestStatusCannotBeMutated(t *testing.T) {
	h := newHarness(t, baseYAML)
	h.save(t, baseYAML+"unknown: 1\n")
	status := h.manager.Status()
	status.Errors[0].Message = "tampered"
	if h.manager.Status().Errors[0].Message == "tampered" {
		t.Fatal("status errors must be copied")
	}
}

func TestShutdownStopsWorkers(t *testing.T) {
	before := runtime.NumGoroutine()
	h := newHarness(t, baseYAML)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.manager.Run(ctx) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop after cancellation")
	}
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > before {
		if time.Now().After(deadline) {
			t.Fatalf("goroutines left behind: %d > %d", runtime.NumGoroutine(), before)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Referenced so the fake watcher in the watcher tests stays honest about the
// interface: a compile error here means the loop's contract changed.
var _ fsWatcher = (*fakeWatcher)(nil)

type fakeWatcher struct {
	mu      sync.Mutex
	adds    int
	removes int
	events  chan fsnotify.Event
	errors  chan error
	closed  bool
}

func newFakeWatcher() *fakeWatcher {
	return &fakeWatcher{events: make(chan fsnotify.Event, 8), errors: make(chan error, 8)}
}

func (w *fakeWatcher) Add(string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.adds++
	return nil
}

func (w *fakeWatcher) Remove(string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.removes++
	return nil
}

func (w *fakeWatcher) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
	return nil
}

func (w *fakeWatcher) Events() <-chan fsnotify.Event { return w.events }
func (w *fakeWatcher) Errors() <-chan error          { return w.errors }
