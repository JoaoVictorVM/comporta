// Package paneltest builds a panel over real storage with seeded data, for the
// panel's own tests, the integration tests and the future demo.
package paneltest

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/admin"
	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/events"
	"github.com/JoaoVictorVM/comporta/internal/limits"
	"github.com/JoaoVictorVM/comporta/internal/panel"
	"github.com/JoaoVictorVM/comporta/internal/panel/ui"
	"github.com/JoaoVictorVM/comporta/internal/sensitivedata"
	"github.com/JoaoVictorVM/comporta/internal/storage"
	"github.com/JoaoVictorVM/comporta/internal/storage/sqlite"
	"github.com/JoaoVictorVM/comporta/internal/useridentity"
)

// Config is a configuration with two apps in different timezones, a budget,
// a downgrade policy and per-user limits.
const Config = `timezone: UTC
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
    limits: {requests_per_minute: 120}
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

// Token is the admin token of the environment.
var Token = strings.Repeat("0123456789abcdef", 4)

type credential struct{}

func (credential) Verify(token string) bool { return token == Token }

// Pause is a settable pause source.
type Pause struct {
	mu    sync.Mutex
	state storage.PauseState
}

func (p *Pause) PauseState() storage.PauseState {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state
}

// Set replaces the pause state.
func (p *Pause) Set(state storage.PauseState) {
	p.mu.Lock()
	p.state = state
	p.mu.Unlock()
}

// Env is a panel over a real SQLite store, identity and limit coordinator.
type Env struct {
	Dir      string
	Manager  *config.Manager
	Store    *sqlite.Store
	Identity *useridentity.Service
	Limits   *limits.Manager
	Pause    *Pause
	Sessions *admin.Sessions
	Service  *panel.Service
	Live     *panel.LiveHub
	Handler  *panel.Handler
	Server   *httptest.Server
}

// New builds an environment and serves its handler on a test server.
func New(t testing.TB) *Env {
	t.Helper()
	return NewWith(t, Config)
}

// NewWith builds an environment from another configuration.
func NewWith(t testing.TB, yaml string) *Env {
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
	coordinator := limits.New(limits.Options{Store: store})
	if err := coordinator.Recover(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	env := &Env{Dir: dir, Manager: manager, Store: store, Identity: identity, Limits: coordinator, Pause: &Pause{}, Sessions: admin.NewSessions(nil)}
	env.Service = panel.NewService(panel.Options{Store: store, Config: manager, Pause: env.Pause, Limits: coordinator, Identity: identity, History: store})
	env.Live = panel.NewLiveHub(panel.LiveOptions{Bus: &store.Bus, Config: manager})
	env.Handler = panel.NewHandler(panel.HandlerOptions{Service: env.Service, Views: ui.Views{}, Credential: credential{}, Sessions: env.Sessions,
		Guard: admin.NewGuard(nil, nil), Live: env.Live})
	env.Server = httptest.NewServer(env.Handler)
	t.Cleanup(env.Server.Close)
	t.Cleanup(env.Live.Shutdown)
	return env
}

// Record is a finished attempt of app by user at the given time.
func (e *Env) Record(t testing.TB, n int, app, user string, at time.Time, cost config.Microusd) storage.Record {
	t.Helper()
	hash, err := e.Identity.LookupHash(app, user)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := e.Manager.Capture()
	a, _ := snapshot.App(app)
	loc, _ := time.LoadLocation(a.Timezone)
	in, out, read := int64(100), int64(30), int64(20)
	model := a.AllowedModels[0]
	provider, _ := snapshot.ProviderForModel(model)
	status := 200
	return storage.Record{RequestID: fmt.Sprintf("%032x", n), AttemptIndex: 1, StartedAt: at, FinishedAt: at.Add(800 * time.Millisecond), AppName: app, UserHash: hash.Hex(),
		Operation: "generation", Provider: provider, ModelRequested: model, ModelUsed: model, Outcome: "ok", Input: &in, Output: &out, CacheRead: &read,
		Cost: cost, ReservedCost: cost + 1000, RequestCount: 1, LatencyUS: 800_000, DownstreamStatus: &status, Day: at.In(loc).Format(time.DateOnly), Timezone: a.Timezone}
}

// Commit makes records durable as the metering worker would.
func (e *Env) Commit(t testing.TB, records ...storage.Record) {
	t.Helper()
	ctx := context.Background()
	for _, r := range records {
		if r.AttemptIndex > 0 {
			if err := e.Store.Admit(ctx, r); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := e.Store.Commit(ctx, records); err != nil {
		t.Fatal(err)
	}
}

// Seed fills every screen: successes, a block, a cache hit, a downgrade, a
// fallback, a loop cut, an incomplete usage, configuration and pause events.
// It returns the raw user identifiers it used.
func Seed(t testing.TB, e *Env) []string {
	t.Helper()
	now := time.Now().UTC()
	users := []string{"customer-1", "customer-2", "customer-3"}
	var records []storage.Record
	for i := 0; i < 9; i++ {
		records = append(records, e.Record(t, i+1, "bot", users[i%3], now.Add(-time.Duration(i+1)*time.Minute), config.Microusd(10_000*(i+1))))
	}
	web := e.Record(t, 20, "web", users[0], now.Add(-30*time.Second), 4_000)
	records = append(records, web)
	cached := e.Record(t, 21, "bot", users[1], now.Add(-40*time.Second), 0)
	cached.Outcome, cached.CacheSaved = "cached", cached.ReservedCost
	records = append(records, cached)
	down := e.Record(t, 22, "bot", users[2], now.Add(-50*time.Second), 3_000)
	down.ModelUsed, down.Outcome, down.PolicyOutcome, down.PolicyLimitName, down.PolicyScope, down.PolicyReason, down.DowngradeSaved = "claude-b", "downgraded", "downgraded", "apps.bot.budget.usd_per_day", "app", "spent", 6_000
	records = append(records, down)
	missing := e.Record(t, 23, "bot", users[0], now.Add(-55*time.Second), 9_000)
	missing.UsageMissing, missing.Input, missing.Output = true, nil, nil
	missing.Detections = sensitivedata.Summary{{DataType: sensitivedata.Email, Count: 1, Action: sensitivedata.ModeRedact}}
	records = append(records, missing)
	failing := e.Record(t, 24, "web", users[1], now.Add(-70*time.Second), 0)
	failing.ErrorClass, failing.ErrorCode = "overload", "provider_overloaded"
	records = append(records, failing)
	e.Commit(t, records...)

	blocked := e.Record(t, 30, "bot", users[2], now.Add(-20*time.Second), 0)
	blocked.AttemptIndex, blocked.Outcome, blocked.RequestCount, blocked.ReservedCost, blocked.Provider = 0, "blocked", 0, 0, ""
	blocked.ErrorClass, blocked.ErrorCode, blocked.BlockReason = "rate_limit", "budget_exceeded", "spent"
	blocked.PolicyOutcome, blocked.PolicyLimitName, blocked.PolicyScope, blocked.PolicyReason = "blocked", "apps.bot.per_user.budget.usd_per_day", "user", "spent"
	e.Commit(t, blocked)

	ctx := context.Background()
	hash, _ := e.Identity.LookupHash("bot", users[0])
	cut := events.NewLoopCut(fmt.Sprintf("%032x", 77), now.Add(-10*time.Second), fmt.Sprintf("%032x", 78), "bot", hash.Hex(), false, events.ReasonSimilarRequests, 10, 30*time.Second, time.Minute)
	paused := true
	pause := events.New(events.Paused, now.Add(-5*time.Second))
	pause.App, pause.Payload.Paused, pause.Payload.Scope = "web", &paused, "app"
	fallback := events.NewFallback(now.Add(-15*time.Second), fmt.Sprintf("%032x", 79), "bot", 2, "claude-a", "claude-b", "529")
	for _, ev := range []events.Event{cut, pause, fallback} {
		if err := e.Store.Publish(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	return users
}

// Login opens a session and returns its cookie.
func (e *Env) Login(t testing.TB) *http.Cookie {
	t.Helper()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ := http.NewRequest(http.MethodPost, e.Server.URL+"/login", strings.NewReader(url.Values{"token": {Token}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", e.Server.URL)
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	for _, c := range res.Cookies() {
		if c.Name == admin.SessionCookie {
			return c
		}
	}
	t.Fatalf("login returned %d without a session", res.StatusCode)
	return nil
}
