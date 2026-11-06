package auth

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/keys"
)

const testConfig = `timezone: UTC
providers:
  anthropic:
    api_format: anthropic
    base_url: https://api.anthropic.com
    api_key: ${COMPORTA_AUTH_TEST_KEY}
    models: [model-a]
apps:
  bot:
    allowed_models: [model-a]
  other:
    allowed_models: [model-a]
`

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)}
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

type fixture struct {
	dir     string
	path    string
	config  *config.Manager
	store   *keys.Store
	manager *keys.Manager
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	t.Setenv("COMPORTA_AUTH_TEST_KEY", "provider-key")
	dir := t.TempDir()
	path := filepath.Join(dir, "comporta.yaml")
	if err := os.WriteFile(path, []byte(testConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := config.NewManager(config.ManagerOptions{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{dir: dir, path: path, config: manager, store: keys.NewStore(manager.Capture().DataDir())}
}

func (f *fixture) create(t *testing.T, app string) keys.Created {
	t.Helper()
	created, err := f.store.Create(context.Background(), app)
	if err != nil {
		t.Fatal(err)
	}
	return created
}

// keyManager loads the store as it is now. Tests call it after their changes, so
// they do not depend on the watcher's timing.
func (f *fixture) keyManager(t *testing.T) *keys.Manager {
	t.Helper()
	manager, err := keys.NewManager(keys.ManagerOptions{Dir: f.config.Capture().DataDir()})
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

func (f *fixture) removeApp(t *testing.T, app string) {
	t.Helper()
	section := "  " + app + ":\n    allowed_models: [model-a]\n"
	if !strings.Contains(testConfig, section) {
		t.Fatalf("no section for %s", app)
	}
	if err := os.WriteFile(f.path, []byte(strings.Replace(testConfig, section, "", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if status := f.config.Reload(context.Background()); status.Result != config.ResultApplied {
		t.Fatalf("reload: %+v", status)
	}
}

func (f *fixture) restoreConfig(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(f.path, []byte(testConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	if status := f.config.Reload(context.Background()); status.Result != config.ResultApplied {
		t.Fatalf("reload: %+v", status)
	}
}
