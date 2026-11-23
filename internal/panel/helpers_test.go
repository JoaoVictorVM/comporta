package panel

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/limits"
	"github.com/JoaoVictorVM/comporta/internal/storage"
	"github.com/JoaoVictorVM/comporta/internal/storage/sqlite"
	"github.com/JoaoVictorVM/comporta/internal/useridentity"
)

const testConfig = `timezone: UTC
providers:
  anthropic:
    api_format: anthropic
    base_url: http://localhost:12345
    models: [claude-a, claude-b]
  openai:
    api_format: openai
    base_url: http://localhost:12346
    models: [gpt-x]
apps:
  bot:
    allowed_models: [claude-a, claude-b]
    timezone: America/Sao_Paulo
    budget: {usd_per_day: 2, usd_per_month: 40}
    on_limit: downgrade
    downgrade_to: claude-b
    per_user:
      budget: {usd_per_day: 0.5}
  web:
    allowed_models: [gpt-x]
    timezone: Asia/Tokyo
pricing:
  claude-a: {input: 3, output: 15, cache_write: 3.75, cache_read: 0.3}
  claude-b: {input: 1, output: 5, cache_write: 1.25, cache_read: 0.1}
  gpt-x: {input: 2, output: 8, cache_write: 0, cache_read: 0.5}
`

type pauseFake struct {
	mu    sync.Mutex
	state storage.PauseState
}

func (p *pauseFake) PauseState() storage.PauseState {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state
}

func (p *pauseFake) set(state storage.PauseState) {
	p.mu.Lock()
	p.state = state
	p.mu.Unlock()
}

type limitsFake struct {
	state    limits.State
	requests []*limits.Subject
}

func (l *limitsFake) State(p limits.Policy, user *limits.Subject) limits.State {
	l.requests = append(l.requests, user)
	out := l.state
	out.App = p.App
	return out
}

type fixtureEnv struct {
	manager  *config.Manager
	store    *sqlite.Store
	identity *useridentity.Service
	pause    *pauseFake
	limits   *limitsFake
	service  *Service
	now      time.Time
}

func newEnv(t *testing.T) *fixtureEnv {
	t.Helper()
	return newEnvWith(t, testConfig, time.Date(2026, 11, 19, 1, 30, 0, 0, time.UTC))
}

func newEnvWith(t *testing.T, yaml string, now time.Time) *fixtureEnv {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "comporta.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := config.NewManager(config.ManagerOptions{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := manager.Capture()
	identity, err := useridentity.Open(context.Background(), useridentity.Options{Dir: snapshot.DataDir()})
	if err != nil {
		t.Fatal(err)
	}
	store, err := sqlite.Open(context.Background(), snapshot.DataDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	env := &fixtureEnv{manager: manager, store: store, identity: identity, pause: &pauseFake{}, limits: &limitsFake{}, now: now}
	env.service = NewService(Options{Store: store, Config: manager, Pause: env.pause, Limits: env.limits, Identity: identity, History: store, Now: func() time.Time { return env.now }})
	return env
}

// record is a finished attempt of app by user at the given time.
func (e *fixtureEnv) record(t *testing.T, n int, app, user string, at time.Time, cost config.Microusd) storage.Record {
	t.Helper()
	hash, err := e.identity.LookupHash(app, user)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := e.manager.Capture()
	a, _ := snapshot.App(app)
	loc, _ := time.LoadLocation(a.Timezone)
	in, out := int64(100), int64(30)
	model := a.AllowedModels[0]
	provider, _ := snapshot.ProviderForModel(model)
	return storage.Record{RequestID: fmt.Sprintf("%032x", n), AttemptIndex: 1, StartedAt: at, FinishedAt: at.Add(800 * time.Millisecond), AppName: app, UserHash: hash.Hex(),
		Operation: "generation", Provider: provider, ModelRequested: model, ModelUsed: model, Outcome: "ok", Input: &in, Output: &out, Cost: cost, ReservedCost: cost + 1000,
		RequestCount: 1, LatencyUS: 800_000, Day: at.In(loc).Format(time.DateOnly), Timezone: a.Timezone}
}

func (e *fixtureEnv) commit(t *testing.T, records ...storage.Record) {
	t.Helper()
	ctx := context.Background()
	for _, r := range records {
		if r.AttemptIndex > 0 {
			if err := e.store.Admit(ctx, r); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := e.store.Commit(ctx, records); err != nil {
		t.Fatal(err)
	}
}
