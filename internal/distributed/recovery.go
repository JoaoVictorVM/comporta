package distributed

import (
	"context"
	"database/sql"
	"errors"
	"hash/fnv"
	"strconv"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/events"
	"github.com/JoaoVictorVM/comporta/internal/limits"
	"github.com/JoaoVictorVM/comporta/internal/storage"
	"github.com/JoaoVictorVM/comporta/internal/storage/postgres"
	redisstore "github.com/JoaoVictorVM/comporta/internal/storage/redis"
)

// ErrRebuildBusy means another replica holds the gate; the rebuild is retried.
var ErrRebuildBusy = errors.New("shared limit state rebuild postponed")

func exclusiveGate(ctx context.Context, db *sql.DB, cluster string, wait time.Duration) (*sql.Tx, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, storage.ErrUnavailable
	}
	if _, err := tx.ExecContext(ctx, `SELECT set_config('lock_timeout',$1,true),pg_advisory_xact_lock($2)::text`, strconv.FormatInt(wait.Milliseconds(), 10)+"ms", postgres.GateKey(cluster)); err != nil {
		tx.Rollback()
		return nil, ErrRebuildBusy
	}
	return tx, nil
}

// RefreshEpoch follows an epoch published by another replica. It never
// rebuilds by itself: only a server identity change or an integrity failure
// does, and then under the exclusive gate.
func (c *Coordinator) RefreshEpoch(ctx context.Context) (bool, error) {
	state, err := postgres.LoadCluster(ctx, c.o.SQL.DB(), c.o.Cluster)
	if err != nil {
		return false, err
	}
	run, err := c.o.Redis.RunID(ctx)
	if err != nil {
		return false, err
	}
	if state.RedisRunID != run || state.Mode == postgres.ModeRecovering {
		return true, nil
	}
	c.UseEpoch(state.Epoch, state.Mode)
	c.o.Redis.Expect(run)
	return false, nil
}

// Rebuild reconstructs the projection from Postgres in a new epoch, under the
// exclusive gate, so no admission or settlement commits meanwhile and the
// ledger is a complete prefix. Buckets come from the checkpoint and the
// ledger after it, open reservations keep their receipts, slots are restored
// only for live owners, and the loop history, which Postgres never had, is
// replaced by a quarantine. The new namespace becomes visible in one write.
func (c *Coordinator) Rebuild(ctx context.Context, quarantine time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	tx, err := exclusiveGate(ctx, c.o.SQL.DB(), c.o.Cluster, 5*time.Second)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	cluster, err := postgres.LoadCluster(ctx, tx, c.o.Cluster)
	if err != nil {
		return err
	}
	previous := cluster.Mode
	now := time.Now().UTC()
	state, err := c.o.SQL.LoadLimits(ctx, now.Add(-time.Minute))
	if err != nil {
		return err
	}
	zones, err := postgres.LoadZones(ctx, tx)
	if err != nil {
		return err
	}
	pause, err := postgres.PauseIn(ctx, tx)
	if err != nil {
		return err
	}
	live, err := postgres.LiveOwners(ctx, tx)
	if err != nil {
		return err
	}
	owners, active, err := openOwners(ctx, tx)
	if err != nil {
		return err
	}
	run, err := c.o.Redis.RunID(ctx)
	if err != nil {
		return err
	}
	type bucketKey struct {
		app, scope, subject string
		anonymous           bool
		dim                 string
		start, end          int64
	}
	buckets := map[bucketKey]*limits.SharedBucket{}
	get := func(app string, scope, subject string, anonymous bool, dim string, start, end int64) *limits.SharedBucket {
		k := bucketKey{app, scope, subject, anonymous, dim, start, end}
		b := buckets[k]
		if b == nil {
			b = &limits.SharedBucket{BudgetBucket: storage.BudgetBucket{AppName: app, Scope: scope, Subject: subject, Anonymous: anonymous, Dimension: dim, WindowStart: start, WindowEnd: end}}
			buckets[k] = b
		}
		return b
	}
	if cp := state.Checkpoint; cp != nil {
		for _, b := range cp.Buckets {
			x := get(b.AppName, b.Scope, b.Subject, b.Anonymous, b.Dimension, b.WindowStart, b.WindowEnd)
			x.Spent, x.Reserved = b.Spent, b.Reserved
		}
		for _, ref := range cp.Metadata.Unknown {
			get(ref.AppName, ref.Scope, ref.Subject, ref.Anonymous, ref.Dimension, ref.WindowStart, ref.WindowEnd).Unknown = true
		}
	}
	for _, e := range state.Ledger {
		for _, d := range e.Deltas {
			x := get(e.AppName, d.Scope, d.Subject, d.Anonymous, d.Dimension, d.WindowStart, d.WindowEnd)
			x.Spent += d.Spent
			x.Reserved += d.Reserved
			if x.Spent < 0 || x.Reserved < 0 {
				return limits.ErrIntegrity
			}
		}
	}
	epoch := postgres.NewEpoch()
	keys := redisstore.NewKeys(c.o.Cluster, epoch)
	client := c.o.Redis.Client()
	pipe := client.Pipeline()
	apps := map[string]bool{}
	for _, b := range buckets {
		subject := redisstore.SubjectKey(b.Subject, b.Anonymous)
		field := redisstore.BucketField(b.Dimension, b.WindowStart, b.WindowEnd)
		pipe.HSet(ctx, keys.Budget(b.AppName, subject), field+"|s", b.Spent, field+"|r", b.Reserved)
		if b.Unknown {
			pipe.HSet(ctx, keys.Budget(b.AppName, subject), field+"|u", "1")
		}
		if subject != redisstore.AppSubject {
			pipe.SAdd(ctx, keys.Subjects(b.AppName), subject)
		}
		apps[b.AppName] = true
	}
	for _, e := range state.RateEvents {
		tokens := e.EstimatedTokens
		if e.ActualTokens != nil {
			tokens = *e.ActualTokens
		}
		flag := "0"
		if e.Active {
			flag = "1"
		}
		value := strconv.FormatInt(e.AdmittedAt.UnixMilli(), 10) + "|" + strconv.FormatInt(tokens, 10) + "|" + flag
		field := e.RequestID + "|" + strconv.Itoa(max(e.AttemptIndex, 1))
		pipe.HSet(ctx, keys.Rate(e.AppName, redisstore.AppSubject), field, value)
		if e.Subject != "" {
			pipe.HSet(ctx, keys.Rate(e.AppName, redisstore.SubjectKey(e.Subject, e.Anonymous)), field, value)
		}
	}
	slot := func(owner, app, subject, request string) {
		if !live[owner] {
			return
		}
		pipe.HSet(ctx, keys.Slots(app, redisstore.AppSubject), request, owner)
		if subject != redisstore.AppSubject {
			pipe.HSet(ctx, keys.Slots(app, subject), request, owner)
		}
		pipe.SAdd(ctx, keys.Owned(owner), redisstore.SlotEntry(app, subject, request))
	}
	for _, open := range state.Open {
		r := open.Reservation
		pipe.HSet(ctx, keys.Receipt(r.RequestID, r.AttemptIndex), "admit", "rebuilt", "app", r.AppName)
		if _, chained := active[r.RequestID]; !chained && r.AttemptIndex == 1 && r.Quote.Operation != "" {
			slot(owners[r.RequestID+":"+strconv.Itoa(r.AttemptIndex)], r.AppName, redisstore.SubjectKey(r.Subject, r.Anonymous), r.RequestID)
		}
	}
	for request, op := range active {
		slot(op.owner, op.app, redisstore.SubjectKey(op.subject, op.anonymous), request)
	}
	for app, z := range zones {
		pipe.HSet(ctx, keys.Zone(app), "active", z.Active, "pending", z.Pending, "activate_at", z.ActivateAt)
	}
	global := "0"
	if pause.Global {
		global = "1"
	}
	fields := []any{"revision", cluster.ControlRevision, "global", global}
	for _, a := range pause.Apps {
		if a.Paused {
			fields = append(fields, "app:"+a.App, "1")
		}
	}
	pipe.HSet(ctx, keys.Pause(), fields...)
	for app := range apps {
		pipe.Incr(ctx, keys.Version(app))
	}
	pipe.HSet(ctx, keys.Meta(), "loop_quarantine", now.Add(quarantine).UnixMilli(), "watermark", now.UnixMilli())
	if _, err := pipe.Exec(ctx); err != nil {
		return redisstore.ErrUnavailable
	}
	_ = c.o.Redis.Fence(ctx, c.o.Cluster, cluster.Epoch)
	publisher := redisstore.NewKeys(c.o.Cluster, epoch)
	if err := client.HSet(ctx, publisher.Meta(), "protocol", "1", "epoch", epoch, "revision", cluster.ConfigRevision, "state", "normal").Err(); err != nil {
		return redisstore.ErrUnavailable
	}
	// Every settlement committed so far is part of the rebuilt state.
	if _, err := tx.ExecContext(ctx, `UPDATE coordination_outbox SET delivered_at=$1 WHERE delivered_at IS NULL`, now.UnixMicro()); err != nil {
		return storage.ErrUnavailable
	}
	old := cluster.Epoch
	cluster.Epoch, cluster.Mode, cluster.RedisRunID, cluster.DegradedSince = epoch, postgres.ModeNormal, run, time.Time{}
	if err := postgres.UpdateCluster(ctx, tx, cluster); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return storage.ErrUnavailable
	}
	c.UseEpoch(epoch, postgres.ModeNormal)
	c.o.Redis.Expect(run)
	c.logger.Info("shared limit state rebuilt from the ledger", "previous_mode", previous)
	if previous == postgres.ModeDegraded {
		seconds := int64(0)
		if since := cluster.DegradedSince; !since.IsZero() {
			seconds = int64(now.Sub(since) / time.Second)
		}
		c.publish(limitEvent(events.LimitStateRecovered, "", now, "", seconds))
	}
	go func() {
		drop, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = c.o.Redis.DropNamespace(drop, c.o.Cluster, old)
	}()
	return nil
}

type activeOperation struct {
	owner, app, subject string
	anonymous           bool
}

func openOwners(ctx context.Context, tx *sql.Tx) (map[string]string, map[string]activeOperation, error) {
	owners := map[string]string{}
	rows, err := tx.QueryContext(ctx, `SELECT request_id,attempt_index,owner_id FROM limit_reservations WHERE state='reserved'`)
	if err != nil {
		return nil, nil, storage.ErrUnavailable
	}
	for rows.Next() {
		var request, owner string
		var attempt int
		if err := rows.Scan(&request, &attempt, &owner); err != nil {
			rows.Close()
			return nil, nil, storage.ErrUnavailable
		}
		owners[request+":"+strconv.Itoa(attempt)] = owner
	}
	rows.Close()
	active := map[string]activeOperation{}
	rows, err = tx.QueryContext(ctx, `SELECT request_id,app_name,subject,anonymous,owner_id FROM request_operations WHERE state='active'`)
	if err != nil {
		return nil, nil, storage.ErrUnavailable
	}
	defer rows.Close()
	for rows.Next() {
		var request string
		var op activeOperation
		if err := rows.Scan(&request, &op.app, &op.subject, &op.anonymous, &op.owner); err != nil {
			return nil, nil, storage.ErrUnavailable
		}
		active[request] = op
	}
	return owners, active, rows.Err()
}

// Takeover recovers what a process left when its lease expired: the CAS on
// its fencing token makes exactly one replica do it. Open reservations are
// settled from the best evidence, or at their reserved value; the journal is
// materialized as recovered usage; its concurrency slots are freed. Nothing is
// ever sent upstream again.
func (c *Coordinator) Takeover(ctx context.Context, dead postgres.Lease) error {
	tx, err := c.o.SQL.DB().BeginTx(ctx, nil)
	if err != nil {
		return storage.ErrUnavailable
	}
	won, err := postgres.FenceLease(ctx, tx, dead)
	if err != nil || !won {
		tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return storage.ErrUnavailable
	}
	if err := c.recoverOwner(ctx, dead.InstanceID); err != nil {
		return err
	}
	c.logger.Info("replica taken over after its lease expired")
	return nil
}

// recoverOwner is idempotent: a reconciler that dies midway is followed by
// another that finds only what is left.
func (c *Coordinator) recoverOwner(ctx context.Context, owner string) error {
	rows, err := c.o.SQL.DB().QueryContext(ctx, `SELECT request_id,attempt_index FROM limit_reservations WHERE owner_id=$1 AND state='reserved'`, owner)
	if err != nil {
		return storage.ErrUnavailable
	}
	type key struct {
		request string
		attempt int
	}
	var open []key
	for rows.Next() {
		var k key
		if err := rows.Scan(&k.request, &k.attempt); err != nil {
			rows.Close()
			return storage.ErrUnavailable
		}
		open = append(open, k)
	}
	rows.Close()
	for _, k := range open {
		res, err := c.o.SQL.LoadReservation(ctx, k.request, k.attempt)
		if err != nil {
			return err
		}
		evidence, err := c.o.SQL.Evidence(ctx, k.request, k.attempt)
		if err != nil {
			return err
		}
		transition, usage := limits.RecoveredUsage(res, evidence)
		st := limits.SettlementFor(res, transition, usage, time.Now().UTC())
		a := &attempt{res: res, degraded: res.Quote.Degraded}
		if err := c.persistEnding(ctx, a, st, nil); err != nil && !errors.Is(err, storage.ErrConflict) {
			return err
		}
	}
	if err := c.o.SQL.RecoverOwner(ctx, owner, time.Now().UTC()); err != nil {
		return err
	}
	return c.o.Redis.ReleaseOwner(ctx, owner)
}

// ReconcileOrphans removes admissions applied in Redis whose SQL commit never
// happened. The operation lock, which the creating transaction held until it
// ended, proves no such transaction is still open before anything is undone.
func (c *Coordinator) ReconcileOrphans(ctx context.Context) error {
	members, err := c.o.Redis.Orphans(ctx, time.Now().Add(-30*time.Second), 100)
	if err != nil {
		return err
	}
	for _, member := range members {
		orphan, ok, err := c.o.Redis.LoadOrphan(ctx, member)
		if err != nil || !ok {
			continue
		}
		res := orphan.Reservation
		if !c.resolveCommit(res, orphan.Digest, orphan.Owner, false) {
			continue
		}
		_ = c.o.Redis.Confirm(ctx, res.RequestID, res.AttemptIndex)
	}
	return nil
}

// checkpointKey elects one replica to fold the ledger.
func checkpointKey(cluster string) int64 {
	h := fnv.New64a()
	h.Write([]byte("checkpoint:" + cluster))
	return int64(h.Sum64())
}

// Checkpoint folds the ledger into budget_state under a short exclusive gate.
// A replica that cannot take the gate or the election lock simply skips.
func (c *Coordinator) Checkpoint(ctx context.Context) error {
	tx, err := exclusiveGate(ctx, c.o.SQL.DB(), c.o.Cluster, 200*time.Millisecond)
	if err != nil {
		return nil
	}
	defer tx.Rollback()
	var elected bool
	if err := tx.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock($1)`, checkpointKey(c.o.Cluster)).Scan(&elected); err != nil || !elected {
		return nil
	}
	if _, err := postgres.FoldCheckpoint(ctx, tx, time.Now().UTC(), nil, nil); err != nil {
		return err
	}
	return tx.Commit()
}
