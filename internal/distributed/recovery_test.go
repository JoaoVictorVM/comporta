//go:build distributed

package distributed

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/limits"
	"github.com/JoaoVictorVM/comporta/internal/loopguard"
	"github.com/JoaoVictorVM/comporta/internal/storage"
	"github.com/JoaoVictorVM/comporta/internal/storage/postgres"
	redisstore "github.com/JoaoVictorVM/comporta/internal/storage/redis"
	"github.com/JoaoVictorVM/comporta/internal/testkit"
)

func count(t testing.TB, r *replica, query string, args ...any) int {
	t.Helper()
	var n int
	if err := r.sql.DB().QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// shortLease gives a replica a lease that expires almost at once, which is
// what a crashed process leaves behind.
func (c *cluster) shortLease(r *replica) {
	lease, err := r.sql.AcquireLease(context.Background(), postgres.NewEpoch(), c.revision.Load(), 700*time.Millisecond)
	if err != nil {
		c.t.Fatal(err)
	}
	r.lease = lease
	r.sql.SetOwner(postgres.Owner{ID: lease.InstanceID, Fence: lease.Fence})
	r.coord.o.Leases = fixedLease{lease}
}

func TestReplicaCrashRecoversOnce(t *testing.T) {
	c := newCluster(t, 2)
	dead, survivor := c.replicas[0], c.replicas[1]
	c.shortLease(dead)
	p := policy(1_000, config.FailClosed)
	p.AppSet.Limits.MaxConcurrent = 1
	if _, _, d, err := dead.admit(t, p, 300); err != nil || !d.Allowed {
		t.Fatal(err)
	}
	// The process dies: no settlement, no materialization, its slot held.
	if _, _, d, _ := survivor.admit(t, p, 1); d.Allowed {
		t.Fatal("the dead process still holds the only slot")
	}
	time.Sleep(time.Second)
	expired, err := survivor.sql.ExpiredLeases(context.Background())
	if err != nil || len(expired) != 1 {
		t.Fatalf("expired leases: %v %v", expired, err)
	}
	for range 2 {
		if err := survivor.coord.Takeover(context.Background(), expired[0]); err != nil {
			t.Fatal(err)
		}
	}
	if err := survivor.coord.recoverOwner(context.Background(), expired[0].InstanceID); err != nil {
		t.Fatal("a second recovery must find nothing left:", err)
	}
	s, reserved := spent(t, survivor)
	if s != 300 || reserved != 0 {
		t.Fatalf("an attempt without evidence is charged its reservation once: spent %d reserved %d", s, reserved)
	}
	if count(t, survivor, `SELECT count(*) FROM requests WHERE recovered AND usage_missing`) != 1 || count(t, survivor, `SELECT count(*) FROM metering_journal`) != 0 {
		t.Fatal("the journal must be materialized as recovered usage")
	}
	if _, _, d, err := survivor.admit(t, p, 1); err != nil || !d.Allowed {
		t.Fatal("the takeover must free the dead process's slot")
	}
}

func TestStaleOwnerCannotAdmit(t *testing.T) {
	c := newCluster(t, 1)
	r := c.replicas[0]
	c.shortLease(r)
	time.Sleep(time.Second)
	if _, err := r.sql.RenewLease(context.Background(), r.lease, 1, LeaseTTL, 0); !errors.Is(err, postgres.ErrLeaseLost) {
		t.Fatalf("an expired lease must never be renewed, got %v", err)
	}
	if _, _, _, err := r.admit(t, policy(1_000, config.FailClosed), 1); !errors.Is(err, limits.ErrUnavailable) {
		t.Fatalf("an owner whose lease expired must not admit, got %v", err)
	}
}

func TestRedisRestartRebuilds(t *testing.T) {
	c := newCluster(t, 2)
	p := policy(1_000, config.FailClosed)
	a, rec, _, err := c.replicas[0].admit(t, p, 300)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.replicas[0].coord.SettleAttempt(context.Background(), a, limits.Usage{Cost: 250, Complete: true}, c.replicas[0].persistFor(rec)); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := c.replicas[1].admit(t, p, 200); err != nil {
		t.Fatal(err)
	}
	if err := c.replicas[0].coord.Checkpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The data lives on tmpfs: a restart loses the whole projection.
	testkit.RestartRedis(t)
	if _, _, _, err := c.replicas[1].admit(t, p, 1); err == nil {
		t.Fatal("a lost projection must never be read as an empty budget")
	}
	rebuild, err := c.replicas[1].coord.RefreshEpoch(context.Background())
	if err != nil || !rebuild {
		t.Fatalf("a new server identity must require a rebuild: %v %v", rebuild, err)
	}
	if err := c.replicas[1].coord.Rebuild(context.Background(), time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := c.replicas[0].coord.RefreshEpoch(context.Background()); err != nil {
		t.Fatal(err)
	}
	s, reserved := spent(t, c.replicas[0])
	if s != 250 || reserved != 200 {
		t.Fatalf("rebuild must restore spend and open reservations: spent %d reserved %d", s, reserved)
	}
	if _, _, d, err := c.replicas[0].admit(t, p, 600); err != nil || d.Allowed {
		t.Fatalf("the rebuilt budget must still refuse what does not fit: %+v %v", d, err)
	}
	if _, _, d, err := c.replicas[0].admit(t, p, 500); err != nil || !d.Allowed {
		t.Fatalf("what fits must be admitted after the rebuild: %v", err)
	}
}

func TestOrphanReservationReconciled(t *testing.T) {
	c := newCluster(t, 1)
	r := c.replicas[0]
	p := policy(1_000, config.FailClosed)
	id := newRequestID()
	plan, err := limits.Decide(p, request(id), candidates(400), limits.SharedState{Now: time.Now().Add(-time.Minute)}, false)
	if err != nil {
		t.Fatal(err)
	}
	snap, _ := r.redis.Read(context.Background(), "bot", limits.Subject{App: "bot", Hash: userHash})
	// Applied in Redis, then the process died before its SQL commit.
	if status, err := r.redis.Admit(context.Background(), redisstore.Admission{Plan: plan, Version: snap.Version, Digest: "lost", Owner: r.lease.InstanceID}); err != nil || status != redisstore.AdmitOK {
		t.Fatal(status, err)
	}
	if _, reserved := spent(t, r); reserved != 400 {
		t.Fatal("orphan not applied")
	}
	if err := r.coord.ReconcileOrphans(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, reserved := spent(t, r); reserved != 0 {
		t.Fatalf("an orphan proven uncommitted must be released, reserved %d", reserved)
	}
}

func TestPauseAcrossReplicasWithinTwoSeconds(t *testing.T) {
	c := newCluster(t, 2)
	p := policy(1_000_000, config.FailClosed)
	a, rec, _, err := c.replicas[1].admit(t, p, 1)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, changed, err := c.replicas[0].coord.Pauses().SavePause(context.Background(), storage.PauseChange{App: "bot", Paused: true, At: time.Now()}); err != nil || !changed {
		t.Fatal(err)
	}
	if _, _, _, err := c.replicas[1].admit(t, p, 1); !errors.Is(err, limits.ErrPaused) {
		t.Fatalf("pause on one replica must block the other, got %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("pause took longer than 2 seconds to apply")
	}
	// What was admitted before keeps running to its end.
	if err := c.replicas[1].coord.SettleAttempt(context.Background(), a, limits.Usage{Cost: 1, Complete: true}, c.replicas[1].persistFor(rec)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.replicas[1].coord.Pauses().SavePause(context.Background(), storage.PauseChange{All: true, Paused: false, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, _, d, err := c.replicas[0].admit(t, p, 1); err != nil || !d.Allowed {
		t.Fatalf("resume all must clear the app override: %v", err)
	}
}

func TestPauseDuringRedisOutage(t *testing.T) {
	c := newCluster(t, 2)
	resume := testkit.PauseRedis(t)
	_, changed, err := c.replicas[0].coord.Pauses().SavePause(context.Background(), storage.PauseChange{All: true, Paused: true, At: time.Now()})
	resume()
	if err != nil || !changed {
		t.Fatalf("a pause must be durable without Redis: %v", err)
	}
	if _, _, _, err := c.replicas[1].admit(t, policy(1_000, config.FailOpen), 1); !errors.Is(err, limits.ErrPaused) {
		t.Fatalf("the projection missed the pause, the gate must not: %v", err)
	}
}

func TestFailOpenRecoveryReconciles(t *testing.T) {
	c := newCluster(t, 2)
	p := policy(1_000, config.FailOpen)
	if _, _, _, err := c.replicas[0].admit(t, p, 100); err != nil {
		t.Fatal(err)
	}
	resume := testkit.PauseRedis(t)
	a, rec, d, err := c.replicas[1].admit(t, p, 300)
	if err != nil || !d.Degraded {
		resume()
		t.Fatalf("degraded admission: %v", err)
	}
	if err := c.replicas[1].coord.SettleAttempt(context.Background(), a, limits.Usage{Cost: 350, Complete: true}, c.replicas[1].persistFor(rec)); err != nil {
		resume()
		t.Fatal(err)
	}
	resume()
	if err := c.replicas[0].coord.Rebuild(context.Background(), time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := c.replicas[1].coord.RefreshEpoch(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, _ := postgres.LoadCluster(context.Background(), c.replicas[0].sql.DB(), c.name)
	if state.Mode != postgres.ModeNormal {
		t.Fatalf("recovery must end in normal mode, got %s", state.Mode)
	}
	s, reserved := spent(t, c.replicas[1])
	if s != 350 || reserved != 100 {
		t.Fatalf("degraded spend must be reapplied: spent %d reserved %d", s, reserved)
	}
	if count(t, c.replicas[0], `SELECT count(*) FROM events WHERE type='limit_state_recovered'`) == 0 {
		t.Fatal("the recovery must be published")
	}
}

func TestForgetPreservesAppSpend(t *testing.T) {
	c := newCluster(t, 2)
	p := policy(1_000_000, config.FailClosed)
	a, rec, _, err := c.replicas[0].admit(t, p, 120)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.replicas[0].coord.SettleAttempt(context.Background(), a, limits.Usage{Cost: 120, Complete: true}, c.replicas[0].persistFor(rec)); err != nil {
		t.Fatal(err)
	}
	done := rec
	done.FinishedAt = done.StartedAt
	in, out := int64(4), int64(6)
	done.Input, done.Output, done.Cost = &in, &out, 120
	if err := c.replicas[0].sql.Commit(context.Background(), []storage.Record{done}); err != nil {
		t.Fatal(err)
	}
	for _, r := range c.replicas {
		ack(t, r)
	}
	history := c.replicas[1].coord.History()
	history.prepare = 3 * time.Second
	go func() {
		// Every live replica acknowledges the deletion's control revision.
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			for _, r := range c.replicas {
				ack(t, r)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()
	counts, err := history.ForgetSubjects(context.Background(), []storage.SubjectRef{{App: "bot", Hash: userHash}})
	if err != nil {
		t.Fatal(err)
	}
	if counts.Requests != 1 {
		t.Fatalf("history not deleted: %+v", counts)
	}
	snap, err := c.replicas[0].redis.Read(context.Background(), "bot", limits.Subject{App: "bot", Hash: userHash})
	if err != nil {
		t.Fatal(err)
	}
	var appSpent, userBuckets int64
	for _, b := range snap.State.Buckets {
		if b.Scope == "app" && b.Dimension == "usd_per_day" {
			appSpent = b.Spent
		}
		if b.Scope == "user" {
			userBuckets++
		}
	}
	if appSpent != 120 || userBuckets != 0 {
		t.Fatalf("app spend must stay and the user must go: app %d user buckets %d", appSpent, userBuckets)
	}
	if count(t, c.replicas[0], `SELECT count(*) FROM deletion_jobs WHERE phase='done' AND subjects IS NULL`) != 1 {
		t.Fatal("the job must end with a receipt that names no one")
	}
}

// ack acknowledges the current control revision, as a replica's lifecycle
// does after purging its caches and flushing its usage queue.
func ack(t testing.TB, r *replica) {
	rev, err := r.coord.ControlRevision(context.Background())
	if err != nil {
		return
	}
	if renewed, err := r.sql.RenewLease(context.Background(), r.lease, 1, time.Minute, rev); err == nil {
		r.lease = renewed
	}
}

func TestForgetBarrierBlocksTheUser(t *testing.T) {
	c := newCluster(t, 2)
	history := c.replicas[0].coord.History()
	if _, err := history.publish(context.Background(), postgres.NewEpoch(), []storage.SubjectRef{{App: "bot", Hash: userHash}}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := c.replicas[1].admit(t, policy(1_000, config.FailClosed), 1); !errors.Is(err, limits.ErrMaintenance) {
		t.Fatalf("a user being deleted must be held back on every replica, got %v", err)
	}
}

func TestLoopAlternatingReplicas(t *testing.T) {
	c := newCluster(t, 2)
	key := make([]byte, 32)
	guards := []*LoopGuard{NewLoopGuard(c.replicas[0].redis, nil, key, nil), NewLoopGuard(c.replicas[1].redis, nil, key, nil)}
	policy := loopguard.Policy{Enabled: true, SimilarRequests: 3, SimilarWindow: time.Minute, RequestsPerMinute: 100, Cooldown: time.Minute}
	subject := loopguard.Subject{App: "bot", Hash: userHash}
	var sig [32]byte
	sig[0] = 7
	var first loopguard.Decision
	for i := range 4 {
		d, err := guards[i%2].Check(loopguard.Request{Subject: subject, RequestID: strconv.Itoa(i), Signature: sig, HasSignature: true, Policy: policy})
		if err != nil {
			t.Fatal(err)
		}
		if i < 3 && !d.Allowed {
			t.Fatalf("arrival %d cut early", i)
		}
		if i == 3 {
			if d.Allowed || !d.First {
				t.Fatal("alternating replicas must not split the count")
			}
			first = d
		}
	}
	retry, err := guards[0].Check(loopguard.Request{Subject: subject, RequestID: "x", Policy: policy})
	if err != nil || retry.Allowed || retry.First || retry.Episode.EventID != first.Episode.EventID {
		t.Fatalf("a retry during the cooldown on another replica must see the same episode: %+v %v", retry, err)
	}
	if n := guards[1].ForgetSubjects([]loopguard.Subject{subject}); n != 1 {
		t.Fatal("forget must clear the loop state")
	}
	if d, _ := guards[1].Check(loopguard.Request{Subject: subject, RequestID: "y", Policy: policy}); !d.Allowed {
		t.Fatal("a forgotten subject starts over")
	}
}

func TestLoopRecoveryQuarantine(t *testing.T) {
	c := newCluster(t, 1)
	if err := c.replicas[0].coord.Rebuild(context.Background(), 30*time.Second); err != nil {
		t.Fatal(err)
	}
	guard := NewLoopGuard(c.replicas[0].redis, nil, make([]byte, 32), nil)
	_, err := guard.Check(loopguard.Request{Subject: loopguard.Subject{App: "bot", Hash: userHash}, RequestID: "a",
		Policy: loopguard.Policy{Enabled: true, SimilarRequests: 3, SimilarWindow: time.Minute, RequestsPerMinute: 100, Cooldown: time.Minute}})
	if !errors.Is(err, redisstore.ErrLoopQuarantine) {
		t.Fatalf("lost loop history must quarantine generations, got %v", err)
	}
}
