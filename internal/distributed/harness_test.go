//go:build distributed

package distributed

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/limits"
	"github.com/JoaoVictorVM/comporta/internal/storage"
	"github.com/JoaoVictorVM/comporta/internal/storage/postgres"
	redisstore "github.com/JoaoVictorVM/comporta/internal/storage/redis"
	"github.com/JoaoVictorVM/comporta/internal/testkit"
)

func TestMain(m *testing.M) { os.Exit(testkit.RunWithDatabases(m)) }

type fixedLease struct{ l postgres.Lease }

func (f fixedLease) Lease() (postgres.Lease, bool) { return f.l, true }

// replica is one coordinator with its own pools, as a separate process has.
type replica struct {
	sql   *postgres.Store
	redis *redisstore.Store
	coord *Coordinator
	lease postgres.Lease
}

type cluster struct {
	t        testing.TB
	dsn      string
	addr     string
	db       int
	name     string
	revision atomic.Int64
	replicas []*replica
}

var clusterSeq atomic.Int64

func newCluster(t testing.TB, n int) *cluster {
	t.Helper()
	c := &cluster{t: t, dsn: testkit.Postgres(t), name: fmt.Sprintf("c%d", clusterSeq.Add(1))}
	c.addr, c.db = testkit.Redis(t)
	c.revision.Store(1)
	for i := range n {
		r := c.start()
		if i == 0 {
			if err := r.redis.Client().FlushDB(context.Background()).Err(); err != nil {
				t.Fatal(err)
			}
		}
		c.replicas = append(c.replicas, r)
	}
	for _, r := range c.replicas {
		c.join(r)
	}
	return c
}

func (c *cluster) start() *replica {
	t := c.t
	sql, err := postgres.Open(context.Background(), postgres.Options{DSN: c.dsn, MaxConnections: 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sql.Close() })
	if err := sql.Migrate(context.Background(), 30*time.Second); err != nil {
		t.Fatal(err)
	}
	redis, err := redisstore.Open(context.Background(), redisstore.Options{Address: c.addr, Database: c.db, PoolSize: 32})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { redis.Close() })
	return &replica{sql: sql, redis: redis}
}

func (c *cluster) join(r *replica) {
	t := c.t
	id := Identity{Revision: c.revision.Load(), ConfigFingerprint: strings.Repeat("a", 64), IdentityFingerprint: strings.Repeat("b", 64)}
	state, err := Bootstrap(context.Background(), r.sql, r.redis, c.name, id)
	if err != nil {
		t.Fatal(err)
	}
	instance := postgres.NewEpoch()
	r.lease, err = r.sql.AcquireLease(context.Background(), instance, id.Revision, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	r.sql.SetOwner(postgres.Owner{ID: instance, Fence: r.lease.Fence})
	r.coord = New(Options{Cluster: c.name, SQL: r.sql, Redis: r.redis, Leases: fixedLease{r.lease}, Revision: c.revision.Load, Publisher: r.sql})
	r.coord.UseEpoch(state.Epoch, state.Mode)
}

const userHash = "11111111111111111111111111111111"

func policy(budget config.Microusd, failMode config.FailMode) limits.Policy {
	return limits.Policy{App: "bot", Timezone: "UTC", FailMode: failMode, OnLimit: config.OnLimitBlock,
		AppSet:  limits.Set{Budget: config.Budget{USDPerDay: budget}, Limits: config.Limits{MaxConcurrent: 1000}},
		UserSet: limits.Set{Limits: config.Limits{MaxConcurrent: 1000}}}
}

var requestSeq atomic.Int64

func newRequestID() string { return fmt.Sprintf("%032x", requestSeq.Add(1)+time.Now().UnixNano()) }

func request(id string) limits.Request {
	return limits.Request{RequestID: id, AttemptIndex: 1, Subject: limits.Subject{App: "bot", Hash: userHash}, Operation: limits.OperationGeneration}
}

func candidates(cost config.Microusd) []limits.Candidate {
	return []limits.Candidate{{ID: limits.CandidateOriginal, Model: "m", EstimatedTokens: 10, ReservedCost: cost}}
}

func record(id string, attempt int, cost config.Microusd) storage.Record {
	now := time.Now().UTC()
	return storage.Record{RequestID: id, AttemptIndex: attempt, StartedAt: now, AppName: "bot", UserHash: userHash, Operation: "generation", Outcome: "ok", ModelRequested: "m", ModelUsed: "m",
		ReservedCost: cost, RequestCount: 1, Day: now.Format("2006-01-02"), Timezone: "UTC", Quote: storage.Quote{EstimatedInput: 4, EstimatedOutput: 6, ReservedCost: cost}}
}

// commitFor is what metering passes: the journal entry and the reservation.
func (r *replica) commitFor(rec storage.Record) limits.Commit {
	return func(ctx context.Context, res storage.Reservation, sel limits.Selection) (int64, error) {
		rec.LimitsDegraded = res.Quote.Degraded
		return r.sql.AdmitReserved(ctx, rec, res)
	}
}

func (r *replica) persistFor(rec storage.Record) limits.Persist {
	return func(ctx context.Context, st storage.Settlement) (int64, error) {
		done := rec
		done.FinishedAt = done.StartedAt
		in, out := int64(4), int64(6)
		done.Input, done.Output, done.Cost = &in, &out, done.ReservedCost
		return r.sql.CompleteSettled(ctx, done, st)
	}
}

func (r *replica) admit(t testing.TB, p limits.Policy, cost config.Microusd) (limits.Attempt, storage.Record, limits.Decision, error) {
	t.Helper()
	id := newRequestID()
	rec := record(id, 1, cost)
	a, d, _, err := r.coord.ReserveAttempt(context.Background(), p, request(id), candidates(cost), r.commitFor(rec))
	return a, rec, d, err
}

func spent(t testing.TB, r *replica) (int64, int64) {
	t.Helper()
	snap, err := r.redis.Read(context.Background(), "bot", limits.Subject{App: "bot", Hash: userHash})
	if err != nil {
		t.Fatal(err)
	}
	var s, res int64
	for _, b := range snap.State.Buckets {
		if b.Scope == "app" && b.Dimension == "usd_per_day" {
			s += b.Spent
			res += b.Reserved
		}
	}
	return s, res
}
