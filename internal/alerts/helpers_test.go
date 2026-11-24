package alerts

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/limits"
	"github.com/JoaoVictorVM/comporta/internal/storage"
	"github.com/JoaoVictorVM/comporta/internal/storage/sqlite"
)

// baseConfig has one app with a daily budget, per-user budget and hard cap,
// a fail_open app and a fail_closed app. The channels block is appended.
const baseConfig = `timezone: UTC
data_dir: %s
providers:
  anthropic:
    api_format: anthropic
    base_url: http://127.0.0.1:9
    models: [fake-claude]
apps:
  bot:
    allowed_models: [fake-claude]
    fail_mode: fail_open
    budget:
      usd_per_day: 2.00
    hard_cap:
      usd_per_day: 3.00
    per_user:
      budget:
        usd_per_day: 1.00
  strict:
    allowed_models: [fake-claude]
    fail_mode: fail_closed
alerts:
%s`

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Set(t time.Time) {
	c.mu.Lock()
	c.now = t.UTC()
	c.mu.Unlock()
}

type env struct {
	t       testing.TB
	dir     string
	path    string
	vars    map[string]string
	manager *config.Manager
	store   *sqlite.Store
	clock   *testClock
	service *Service
	limits  *limits.Manager
	fake    *limits.FakeClock
	seq     atomic.Int64
}

var day = time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC)

func newEnv(t testing.TB, alerts string, vars map[string]string, options ...func(*Options)) *env {
	t.Helper()
	e := &env{t: t, dir: t.TempDir(), vars: vars, clock: &testClock{now: day.Add(12 * time.Hour)}}
	e.path = filepath.Join(e.dir, "comporta.yaml")
	e.write(alerts)
	manager, err := config.NewManager(config.ManagerOptions{Path: e.path, Options: config.Options{Environment: config.NewEnvironment(vars)}})
	if err != nil {
		t.Fatal(err)
	}
	e.manager = manager
	store, err := sqlite.Open(context.Background(), filepath.Join(e.dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	e.store = store
	e.fake = limits.NewFakeClock(e.clock.Now())
	e.limits = limits.New(limits.Options{Store: store, Clock: e.fake})
	if err := e.limits.Recover(context.Background(), manager.Capture()); err != nil {
		t.Fatal(err)
	}
	opts := Options{Store: store, Source: manager, Now: e.clock.Now, Poll: 10 * time.Millisecond}
	for _, apply := range options {
		apply(&opts)
	}
	e.service = New(opts)
	return e
}

func (e *env) write(alerts string) {
	e.t.Helper()
	data := fmt.Sprintf(baseConfig, filepath.ToSlash(filepath.Join(e.dir, "data")), alerts)
	if err := os.WriteFile(e.path, []byte(data), 0o600); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) reload(alerts string) {
	e.t.Helper()
	e.write(alerts)
	before := e.manager.Capture().Generation()
	e.manager.Reload(context.Background())
	if e.manager.Capture().Generation() == before {
		e.t.Fatalf("reload not applied: %+v", e.manager.Status())
	}
}

// at moves both clocks to the same civil instant.
func (e *env) at(t time.Time) {
	e.clock.Set(t)
	e.fake.SetCivil(t)
}

func (e *env) record(user string, cost int64) storage.Record {
	n := e.seq.Add(1)
	now := e.clock.Now()
	in := int64(10)
	return storage.Record{RequestID: fmt.Sprintf("%032x", n), AttemptIndex: 1, StartedAt: now, FinishedAt: now, AppName: "bot", UserHash: user,
		Operation: "generation", Outcome: "ok", Input: &in, Cost: config.Microusd(cost), ReservedCost: 1000, RequestCount: 1,
		Day: now.Format("2006-01-02"), Timezone: "UTC"}
}

// spend reserves, settles and materializes one request of a user, exactly as
// the gateway does, so the alert sees confirmed spend and a completion event.
// The reservation is small and the settlement carries the real cost, like an
// answer longer than its estimate, so spend can jump past a budget.
func (e *env) spend(user string, cost int64) storage.Record {
	e.t.Helper()
	ctx := context.Background()
	r := e.record(user, cost)
	policy, _ := limits.PolicyFor(e.manager.Capture(), "bot")
	request := limits.Request{RequestID: r.RequestID, AttemptIndex: 1, Subject: limits.Subject{App: "bot", Hash: user}, Operation: limits.OperationGeneration, EstimatedTokens: 10, ReservedCost: r.ReservedCost}
	h, d, err := e.limits.Reserve(policy, request, func(res storage.Reservation) (int64, error) { return e.store.AdmitReserved(ctx, r, res) })
	if err != nil || !d.Allowed {
		e.t.Fatalf("reserve: %v %+v", err, d.Primary)
	}
	if err = e.limits.Settle(h, limits.Usage{Cost: r.Cost, Tokens: 10, Complete: true}, func(st storage.Settlement) (int64, error) {
		return e.store.CompleteSettled(ctx, r, st)
	}); err != nil {
		e.t.Fatal(err)
	}
	if err = e.store.Commit(ctx, []storage.Record{r}); err != nil {
		e.t.Fatal(err)
	}
	return r
}

func (e *env) process() {
	e.t.Helper()
	for {
		n, err := e.service.ProcessInputs(context.Background())
		if err != nil {
			e.t.Fatal(err)
		}
		if n == 0 {
			return
		}
	}
}

func (e *env) occurrences() []storage.AlertOccurrence {
	e.t.Helper()
	out, err := e.store.RecentAlertOccurrences(context.Background(), 100)
	if err != nil {
		e.t.Fatal(err)
	}
	return out
}

func (e *env) byTrigger(trigger Trigger) []Payload {
	e.t.Helper()
	var out []Payload
	for _, o := range e.occurrences() {
		if o.Trigger != string(trigger) {
			continue
		}
		p, err := DecodePayload(o.Payload)
		if err != nil {
			e.t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

const (
	userA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	userB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)
