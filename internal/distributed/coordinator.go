// Package distributed coordinates admissions across replicas. Postgres holds
// the durable gate, journal and ledger; Redis holds the projection on which
// decisions are made. No transaction spans both, so the protocol orders the
// steps to keep every intermediate state conservative:
//
//  1. a Postgres transaction takes the shared control gate and the app lock;
//  2. the app's state is read from Redis and decided with the local rules;
//  3. the plan is applied in Redis only if the app did not change meanwhile;
//  4. the journal, reservation and ledger rows commit in the same transaction.
//
// Only after step 4 succeeds does the proxy get an authorization to send. A
// settlement commits in Postgres first, with an outbox row, and is projected
// in Redis afterwards; until then the reservation keeps the money held.
package distributed

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/events"
	"github.com/JoaoVictorVM/comporta/internal/limits"
	"github.com/JoaoVictorVM/comporta/internal/storage"
	"github.com/JoaoVictorVM/comporta/internal/storage/postgres"
	redisstore "github.com/JoaoVictorVM/comporta/internal/storage/redis"
)

// AdmissionDeadline bounds one admission, Redis and Postgres included.
const AdmissionDeadline = 2 * time.Second

// redisCall bounds one script call inside the admission deadline.
const redisCall = 500 * time.Millisecond

// LeaseSource gives the lease this process admits under.
type LeaseSource interface {
	Lease() (postgres.Lease, bool)
}

// Options configures a Coordinator.
type Options struct {
	Cluster string
	SQL     *postgres.Store
	Redis   *redisstore.Store
	Leases  LeaseSource
	// Revision is the configuration revision this process runs.
	Revision  func() int64
	Logger    *slog.Logger
	Publisher events.Publisher
	// Recover is signalled when the projection must be rebuilt.
	Recover func(reason string)
}

// Coordinator is the distributed implementation of limits.Coordinator.
type Coordinator struct {
	o      Options
	epoch  atomic.Pointer[string]
	mode   atomic.Pointer[string]
	logger *slog.Logger

	mu    sync.Mutex
	retry []*ending
	slots []slotRelease
}

var _ limits.Coordinator = (*Coordinator)(nil)

// New returns a coordinator for an epoch already bootstrapped or rebuilt.
func New(o Options) *Coordinator {
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.Recover == nil {
		o.Recover = func(string) {}
	}
	c := &Coordinator{o: o, logger: o.Logger}
	normal := postgres.ModeNormal
	c.mode.Store(&normal)
	return c
}

// UseLeases sets the lease source, which exists only once the lifecycle
// started. Until then nothing is admitted.
func (c *Coordinator) UseLeases(l LeaseSource) { c.o.Leases = l }

// UseEpoch switches the coordinator and the Redis client to an epoch.
func (c *Coordinator) UseEpoch(epoch, mode string) {
	c.epoch.Store(&epoch)
	c.mode.Store(&mode)
	c.o.Redis.UseEpoch(c.o.Cluster, epoch)
}

func (c *Coordinator) currentEpoch() string {
	if e := c.epoch.Load(); e != nil {
		return *e
	}
	return ""
}

// Mode is the cluster mode last observed in a gate.
func (c *Coordinator) Mode() string { return *c.mode.Load() }

type attempt struct {
	res       storage.Reservation
	selection limits.Selection
	degraded  bool
	digest    string
	owner     string
	chain     *chain
	slot      bool

	mu    sync.Mutex
	ended bool
}

func (a *attempt) Degraded() bool { return a != nil && a.degraded }

func (a *attempt) Selection() limits.Selection {
	if a == nil {
		return limits.Selection{}
	}
	return a.selection
}

type chain struct {
	policy  limits.Policy
	first   limits.Request
	subject limits.Subject
	owner   string
	slot    bool

	mu     sync.Mutex
	last   *attempt
	closed bool
}

func (ch *chain) RequestID() string { return ch.first.RequestID }

func digestOf(res storage.Reservation, sel limits.Selection) string {
	data, _ := json.Marshal(struct {
		R storage.Reservation
		C limits.CandidateID
		M string
		O limits.PolicyOutcome
	}{res, sel.Candidate, sel.Model, sel.Outcome})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:16])
}

// errRetryDegraded restarts an admission after the cluster became degraded.
var errRetryDegraded = errors.New("cluster mode changed")

func (c *Coordinator) ReserveAttempt(ctx context.Context, p limits.Policy, r limits.Request, candidates []limits.Candidate, commit limits.Commit) (limits.Attempt, limits.Decision, limits.Selection, error) {
	a, d, s, err := c.admit(ctx, p, r, candidates, commit, nil)
	if a == nil {
		return nil, d, s, err
	}
	return a, d, s, err
}

func (c *Coordinator) OpenChain(ctx context.Context, p limits.Policy, r limits.Request, candidates []limits.Candidate, commit limits.Commit) (limits.Chain, limits.Attempt, limits.Decision, limits.Selection, error) {
	if r.AttemptIndex != 1 {
		return nil, nil, limits.Decision{}, limits.Selection{}, limits.ErrIntegrity
	}
	ch := &chain{policy: p, first: r, subject: r.Subject}
	a, d, s, err := c.admit(ctx, p, r, candidates, commit, ch)
	if a == nil {
		return nil, nil, d, s, err
	}
	ch.last, ch.owner, ch.slot = a, a.owner, a.slot
	return ch, a, d, s, err
}

func (c *Coordinator) ContinueChain(ctx context.Context, op limits.Chain, r limits.Request, candidates []limits.Candidate, commit limits.Commit) (limits.Attempt, limits.Decision, limits.Selection, error) {
	ch, _ := op.(*chain)
	if ch == nil {
		return nil, limits.Decision{}, limits.Selection{}, limits.ErrConflict
	}
	ch.mu.Lock()
	last, closed := ch.last, ch.closed
	ch.mu.Unlock()
	if closed || last == nil {
		return nil, limits.Decision{}, limits.Selection{}, limits.ErrConflict
	}
	last.mu.Lock()
	ended := last.ended
	last.mu.Unlock()
	// The next attempt starts only once the previous one is settled durably.
	if !ended || r.RequestID != ch.first.RequestID || r.Subject != ch.first.Subject || r.Operation != ch.first.Operation ||
		r.AttemptIndex != last.res.AttemptIndex+1 || r.AttemptIndex > storage.MaxAttempts {
		return nil, limits.Decision{}, limits.Selection{}, limits.ErrConflict
	}
	r.Generation = ch.first.Generation
	a, d, s, err := c.admit(ctx, ch.policy, r, candidates, commit, ch)
	if a == nil {
		return nil, d, s, err
	}
	ch.mu.Lock()
	ch.last = a
	ch.mu.Unlock()
	return a, d, s, err
}

func (c *Coordinator) CloseChain(op limits.Chain) {
	ch, _ := op.(*chain)
	if ch == nil {
		return
	}
	ch.mu.Lock()
	if ch.closed {
		ch.mu.Unlock()
		return
	}
	ch.closed = true
	ch.mu.Unlock()
	if ch.slot {
		c.releaseSlot(ch.owner, ch.policy.App, ch.subject, ch.first.RequestID)
	}
}

// admit runs the admission protocol, restarting once if the cluster mode
// changed under it.
func (c *Coordinator) admit(ctx context.Context, p limits.Policy, r limits.Request, candidates []limits.Candidate, commit limits.Commit, ch *chain) (*attempt, limits.Decision, limits.Selection, error) {
	for range 2 {
		a, d, s, err := c.admitOnce(ctx, p, r, candidates, commit, ch)
		if !errors.Is(err, errRetryDegraded) {
			return a, d, s, err
		}
	}
	return nil, limits.Decision{Degraded: true}, limits.Selection{}, limits.ErrUnavailable
}

// sharedGate opens a transaction holding the shared control gate, with
// bounded lock waits: a lock is never awaited past the admission deadline,
// and a transaction left open by a dead client is cut by the server.
func (c *Coordinator) sharedGate(ctx context.Context) (*sql.Tx, error) {
	tx, err := c.o.SQL.DB().BeginTx(ctx, nil)
	if err != nil {
		return nil, storage.ErrUnavailable
	}
	if _, err := tx.ExecContext(ctx, `SELECT set_config('lock_timeout','2s',true),set_config('idle_in_transaction_session_timeout','10s',true),pg_advisory_xact_lock_shared($1)::text`, postgres.GateKey(c.o.Cluster)); err != nil {
		tx.Rollback()
		return nil, storage.ErrUnavailable
	}
	return tx, nil
}

// gate opens an admission or settlement transaction: shared control gate,
// cluster checks, then the app lock. Locks are always taken in this order.
func (c *Coordinator) gate(ctx context.Context, app string, exclusiveApp bool) (*sql.Tx, postgres.ClusterState, error) {
	tx, err := c.o.SQL.DB().BeginTx(ctx, nil)
	if err != nil {
		return nil, postgres.ClusterState{}, storage.ErrUnavailable
	}
	fail := func(err error) (*sql.Tx, postgres.ClusterState, error) {
		tx.Rollback()
		return nil, postgres.ClusterState{}, err
	}
	// Bounded waits: a lock is never awaited past the admission deadline, and a
	// transaction left open by a dead client is cut by the server.
	if _, err := tx.ExecContext(ctx, `SET LOCAL lock_timeout='2s'; SET LOCAL idle_in_transaction_session_timeout='10s'`); err != nil {
		return fail(storage.ErrUnavailable)
	}
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock_shared($1)`, postgres.GateKey(c.o.Cluster)); err != nil {
		return fail(storage.ErrUnavailable)
	}
	cluster, err := postgres.LoadCluster(ctx, tx, c.o.Cluster)
	if err != nil {
		return fail(storage.ErrUnavailable)
	}
	if exclusiveApp {
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, postgres.AppKey(c.o.Cluster, app)); err != nil {
			return fail(storage.ErrUnavailable)
		}
	}
	return tx, cluster, nil
}

func (c *Coordinator) admitOnce(ctx context.Context, p limits.Policy, r limits.Request, candidates []limits.Candidate, commit limits.Commit, ch *chain) (*attempt, limits.Decision, limits.Selection, error) {
	ctx, cancel := context.WithTimeout(ctx, AdmissionDeadline)
	defer cancel()
	if c.o.Leases == nil {
		return nil, limits.Decision{}, limits.Selection{}, limits.ErrUnavailable
	}
	lease, ok := c.o.Leases.Lease()
	if !ok {
		return nil, limits.Decision{}, limits.Selection{}, limits.ErrUnavailable
	}
	tx, err := c.sharedGate(ctx)
	if err != nil {
		return nil, limits.Decision{}, limits.Selection{}, err
	}
	committed := false
	defer func() {
		if !committed {
			tx.Rollback()
		}
	}()
	view, err := postgres.AdmissionGate(ctx, tx, c.o.Cluster, p.App, r.RequestID, r.Subject.Hash, lease)
	if err != nil {
		return nil, limits.Decision{}, limits.Selection{}, storage.ErrUnavailable
	}
	cluster := postgres.ClusterState{Epoch: view.Epoch, Mode: view.Mode, ConfigRevision: view.ConfigRevision}
	mode := cluster.Mode
	c.mode.Store(&mode)
	switch {
	case cluster.Epoch != c.currentEpoch():
		c.o.Recover("epoch")
		return nil, limits.Decision{}, limits.Selection{}, limits.ErrUnavailable
	case cluster.ConfigRevision != c.o.Revision():
		// A replica on another revision stops admitting until it gets the
		// configuration in force; it never mixes ceilings with the others.
		return nil, limits.Decision{}, limits.Selection{}, limits.ErrStale
	case cluster.Mode == postgres.ModeRecovering, !view.LeaseValid:
		return nil, limits.Decision{}, limits.Selection{}, limits.ErrUnavailable
	case view.GlobalPaused || view.AppPaused:
		return nil, limits.Decision{}, limits.Selection{}, limits.ErrPaused
	case view.Maintenance && !r.Subject.Anonymous:
		return nil, limits.Decision{}, limits.Selection{}, limits.ErrMaintenance
	}
	continuation := ch != nil && r.AttemptIndex > 1
	var plan limits.Plan
	var digest string
	degraded := cluster.Mode == postgres.ModeDegraded
	if degraded {
		plan, err = limits.Decide(p, r, candidates, limits.SharedState{Now: time.Now().UTC(), Degraded: true}, continuation)
		if err != nil || plan.Reservation.RequestID == "" {
			return nil, plan.Decision, plan.Selection, err
		}
		plan.Slot = false
	} else {
		if view.PendingOutbox {
			if err := c.replay(ctx, tx, p.App); err != nil {
				return c.redisFailure(ctx, p, err)
			}
		}
		plan, digest, err = c.decideAndApply(ctx, p, r, candidates, continuation, lease.InstanceID)
		if err != nil || plan.Reservation.RequestID == "" {
			if isRedisFailure(err) {
				return c.redisFailure(ctx, p, err)
			}
			return nil, plan.Decision, plan.Selection, err
		}
	}
	res := plan.Reservation
	scope := &postgres.Scope{Tx: tx}
	sqlCtx := postgres.WithScope(ctx, scope)
	if continuation {
		sqlCtx = postgres.WithOperationFence(sqlCtx, postgres.OperationFence{Epoch: cluster.Epoch, Revision: cluster.ConfigRevision})
	}
	_, err = commit(sqlCtx, res, plan.Selection)
	if err == nil && plan.Zone != nil {
		err = postgres.SaveZone(ctx, tx, p.App, *plan.Zone)
	}
	if err != nil {
		tx.Rollback()
		committed = true
		c.undo(digest, lease.InstanceID, res, degraded)
		return nil, plan.Decision, limits.Selection{}, err
	}
	if err := tx.Commit(); err != nil {
		committed = true
		if !c.resolveCommit(res, digest, lease.InstanceID, degraded) {
			return nil, plan.Decision, limits.Selection{}, storage.ErrUnavailable
		}
	}
	committed = true
	scope.Committed()
	if !degraded {
		confirm, cancel := context.WithTimeout(context.Background(), redisCall)
		_ = c.o.Redis.Confirm(confirm, res.RequestID, res.AttemptIndex)
		cancel()
	}
	a := &attempt{res: res, selection: plan.Selection, degraded: degraded, digest: digest, owner: lease.InstanceID, chain: ch, slot: plan.Slot}
	decision := plan.Decision
	decision.Degraded = degraded
	if degraded {
		decision.Budget = nil
	}
	return a, decision, plan.Selection, nil
}

func paused(state storage.PauseState, app string) bool {
	for _, entry := range state.Apps {
		if entry.App == app && entry.Paused {
			return true
		}
	}
	return false
}

func isRedisFailure(err error) bool {
	return errors.Is(err, redisstore.ErrUnavailable) || errors.Is(err, redisstore.ErrFenced) || errors.Is(err, redisstore.ErrIntegrity)
}

// decideAndApply reads, decides and applies, retrying when another admission
// of the same app changed it in between. A lost reply is retried with the same
// plan, whose digest makes a second application a confirmation.
func (c *Coordinator) decideAndApply(ctx context.Context, p limits.Policy, r limits.Request, candidates []limits.Candidate, continuation bool, owner string) (limits.Plan, string, error) {
	for ctx.Err() == nil {
		call, cancel := context.WithTimeout(ctx, redisCall)
		snap, err := c.o.Redis.Read(call, p.App, r.Subject)
		cancel()
		if err != nil {
			return limits.Plan{}, "", err
		}
		if snap.Paused {
			return limits.Plan{}, "", limits.ErrPaused
		}
		plan, err := limits.Decide(p, r, candidates, snap.State, continuation)
		if err != nil || plan.Reservation.RequestID == "" {
			return plan, "", err
		}
		digest := digestOf(plan.Reservation, plan.Selection)
		admission := redisstore.Admission{Plan: plan, Version: snap.Version, Digest: digest, Owner: owner}
		status, err := c.applyOnce(ctx, admission)
		if err != nil {
			return limits.Plan{}, "", err
		}
		switch status {
		case redisstore.AdmitOK, redisstore.AdmitDuplicate:
			if plan.Activated {
				if err := c.seedApp(ctx, p, plan); err != nil {
					return limits.Plan{}, "", err
				}
			}
			return plan, digest, nil
		case redisstore.AdmitPaused:
			return limits.Plan{}, "", limits.ErrPaused
		case redisstore.AdmitConflict:
			return limits.Plan{}, "", limits.ErrConflict
		}
	}
	return limits.Plan{}, "", storage.ErrUnavailable
}

// applyOnce sends a plan, and resends exactly the same plan once if the reply
// was lost within the deadline.
func (c *Coordinator) applyOnce(ctx context.Context, admission redisstore.Admission) (redisstore.AdmitStatus, error) {
	call, cancel := context.WithTimeout(ctx, redisCall)
	status, err := c.o.Redis.Admit(call, admission)
	cancel()
	if !errors.Is(err, redisstore.ErrUnavailable) || ctx.Err() != nil {
		return status, err
	}
	call, cancel = context.WithTimeout(ctx, redisCall)
	defer cancel()
	return c.o.Redis.Admit(call, admission)
}

// redisFailure applies the fail mode. fail_closed refuses; fail_open moves
// the whole cluster to degraded under the exclusive gate, so every replica
// reads the same mode, and restarts the admission. Integrity failures and a
// fenced epoch never fail open: they lead to a rebuild.
func (c *Coordinator) redisFailure(ctx context.Context, p limits.Policy, err error) (*attempt, limits.Decision, limits.Selection, error) {
	if errors.Is(err, limits.ErrPaused) || errors.Is(err, limits.ErrConflict) {
		return nil, limits.Decision{}, limits.Selection{}, err
	}
	if !errors.Is(err, redisstore.ErrUnavailable) {
		c.o.Recover("projection")
		return nil, limits.Decision{}, limits.Selection{}, limits.ErrIntegrity
	}
	if p.FailMode != config.FailOpen {
		return nil, limits.Decision{Degraded: true}, limits.Selection{}, limits.ErrUnavailable
	}
	if err := c.promoteDegraded(context.WithoutCancel(ctx), p.App); err != nil {
		return nil, limits.Decision{Degraded: true}, limits.Selection{}, limits.ErrUnavailable
	}
	return nil, limits.Decision{}, limits.Selection{}, errRetryDegraded
}

func (c *Coordinator) promoteDegraded(ctx context.Context, app string) error {
	ctx, cancel := context.WithTimeout(ctx, AdmissionDeadline)
	defer cancel()
	tx, err := c.o.SQL.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SET LOCAL lock_timeout='2s'`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, postgres.GateKey(c.o.Cluster)); err != nil {
		return err
	}
	cluster, err := postgres.LoadCluster(ctx, tx, c.o.Cluster)
	if err != nil {
		return err
	}
	if cluster.Mode != postgres.ModeNormal {
		return tx.Commit()
	}
	cluster.Mode, cluster.DegradedSince = postgres.ModeDegraded, time.Now().UTC()
	if err := postgres.UpdateCluster(ctx, tx, cluster); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	c.logger.Warn("shared limit state unavailable; cluster degraded, applying each app's fail mode")
	c.publish(limitEvent(events.LimitStateUnavailable, "", cluster.DegradedSince, "redis_unavailable", 0))
	c.publish(limitEvent(events.LimitStateUnavailable, app, cluster.DegradedSince, "", 0))
	return nil
}

func limitEvent(kind events.Type, app string, at time.Time, reason string, seconds int64) events.Event {
	e := events.New(kind, at)
	e.App = app
	e.Payload.Severity = "warning"
	if kind == events.LimitStateRecovered {
		e.Payload.Severity = "info"
	}
	e.Payload.Reason = reason
	e.Payload.DegradedSeconds = seconds
	return e
}

func (c *Coordinator) publish(e events.Event) {
	if c.o.Publisher == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = c.o.Publisher.Publish(ctx, e)
}

// undo removes a projection whose SQL transaction was rolled back for sure:
// the error came before the commit was sent.
func (c *Coordinator) undo(digest, owner string, res storage.Reservation, degraded bool) {
	if degraded || digest == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), redisCall)
	defer cancel()
	if err := c.o.Redis.Rollback(ctx, owner, digest, res); err != nil {
		// The orphan watch removes it later, once no creating transaction can
		// still be open; until then it only holds budget conservatively.
		c.logger.Warn("admission projection kept for reconciliation")
	}
}

// resolveCommit decides an ambiguous commit. A new transaction waits on the
// operation lock, which the original transaction held until it ended, so the
// absence of the row afterwards proves a rollback.
func (c *Coordinator) resolveCommit(res storage.Reservation, digest, owner string, degraded bool) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tx, err := c.o.SQL.DB().BeginTx(ctx, nil)
	if err != nil {
		return false
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, postgres.OperationKey(c.o.Cluster, res.RequestID)); err != nil {
		return false
	}
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM limit_reservations WHERE request_id=$1 AND attempt_index=$2)`, res.RequestID, res.AttemptIndex).Scan(&exists); err != nil {
		return false
	}
	if !exists {
		c.undo(digest, owner, res, degraded)
	}
	return exists
}

// replay projects the settlements of this app committed in Postgres but not
// yet in Redis, before deciding: an unprojected settlement may hold spend the
// projection does not show yet.
func (c *Coordinator) replay(ctx context.Context, tx *sql.Tx, app string) error {
	pending, err := postgres.PendingOutboxFor(ctx, tx, app, 100)
	if err != nil {
		return storage.ErrUnavailable
	}
	if len(pending) == 100 {
		return redisstore.ErrUnavailable
	}
	for _, entry := range pending {
		if err := c.project(ctx, entry); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE coordination_outbox SET delivered_at=$1 WHERE id=$2`, time.Now().UTC().UnixMicro(), entry.ID); err != nil {
			return storage.ErrUnavailable
		}
	}
	return nil
}

// seedApp applies a timezone activation to every subject of the app, so a
// user first seen after the change still finds the spend of the old windows.
func (c *Coordinator) seedApp(ctx context.Context, p limits.Policy, plan limits.Plan) error {
	res := plan.Reservation
	return c.o.Redis.SeedApp(ctx, p.App, plan.Zone.Active, res.AdmittedAt, redisstore.SubjectKey(res.Subject, res.Anonymous))
}

// slotRelease is a concurrency slot whose release failed and is retried.
type slotRelease struct {
	owner, app string
	subject    limits.Subject
	request    string
}

func (c *Coordinator) releaseSlot(owner, app string, subject limits.Subject, request string) {
	ctx, cancel := context.WithTimeout(context.Background(), redisCall)
	defer cancel()
	if err := c.o.Redis.ReleaseSlot(ctx, owner, app, subject, request); err != nil {
		c.mu.Lock()
		c.slots = append(c.slots, slotRelease{owner, app, subject, request})
		c.mu.Unlock()
	}
}

// ending is a settlement whose durable write failed and is retried.
type ending struct {
	a  *attempt
	st storage.Settlement
}

func (c *Coordinator) SettleAttempt(ctx context.Context, a limits.Attempt, u limits.Usage, persist limits.Persist) error {
	if u.Cost < 0 || u.Tokens < 0 {
		return limits.ErrIntegrity
	}
	return c.end(ctx, a, storage.TransitionSettle, u, persist)
}

func (c *Coordinator) ReleaseAttempt(ctx context.Context, a limits.Attempt, _ limits.ReleaseReason, persist limits.Persist) error {
	return c.end(ctx, a, storage.TransitionRelease, limits.Usage{}, persist)
}

func (c *Coordinator) end(ctx context.Context, x limits.Attempt, transition string, u limits.Usage, persist limits.Persist) error {
	a, _ := x.(*attempt)
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ended {
		return nil
	}
	// A plain admission holds its own slot; a chain holds one for all attempts.
	if a.slot && a.chain == nil {
		a.slot = false
		c.releaseSlot(a.owner, a.res.AppName, limits.SubjectOf(a.res), a.res.RequestID)
	}
	st := limits.SettlementFor(a.res, transition, u, time.Now().UTC())
	if err := c.persistEnding(ctx, a, st, persist); err != nil {
		// The obligation stays reserved, which is conservative; the worker
		// retries the settlement alone.
		c.mu.Lock()
		c.retry = append(c.retry, &ending{a: a, st: st})
		c.mu.Unlock()
		return err
	}
	a.ended = true
	return nil
}

// outboxPayload carries everything a projection replay needs, so any replica
// can apply it without the attempt in memory.
type outboxPayload struct {
	Reservation storage.Reservation `json:"reservation"`
	Settlement  storage.Settlement  `json:"settlement"`
	Digest      string              `json:"digest"`
	Subject     string              `json:"subject"`
}

// OperationID names the outbox rows of one attempt.
func OperationID(request string, attempt int) string {
	return request + ":" + strconv.Itoa(attempt)
}

func (c *Coordinator) persistEnding(ctx context.Context, a *attempt, st storage.Settlement, persist limits.Persist) error {
	call, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	tx, _, err := c.gate(call, a.res.AppName, true)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	scope := &postgres.Scope{Tx: tx}
	sqlCtx := postgres.WithScope(call, scope)
	if persist != nil {
		_, err = persist(sqlCtx, st)
	} else {
		_, err = c.o.SQL.SettleReservation(sqlCtx, st)
	}
	if err != nil {
		return err
	}
	payload := outboxPayload{Reservation: a.res, Settlement: st, Digest: endingDigest(st), Subject: a.res.Subject}
	var outbox int64
	if !a.degraded {
		if outbox, err = postgres.PutOutbox(call, tx, OperationID(st.RequestID, st.AttemptIndex), st.Transition, a.res.AppName, payload); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return storage.ErrUnavailable
	}
	scope.Committed()
	if a.degraded {
		return nil
	}
	entry := postgres.OutboxEntry{ID: outbox, Transition: st.Transition, AppName: a.res.AppName}
	entry.Payload, _ = json.Marshal(payload)
	if err := c.project(call, entry); err == nil {
		_ = c.o.SQL.MarkDelivered(call, outbox)
	}
	return nil
}

func endingDigest(st storage.Settlement) string {
	data, _ := json.Marshal(st)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:16])
}

// project applies one outbox transition to Redis. Repeats are confirmations.
func (c *Coordinator) project(ctx context.Context, entry postgres.OutboxEntry) error {
	switch entry.Transition {
	case storage.TransitionSettle, storage.TransitionRelease:
	default:
		return nil
	}
	var payload outboxPayload
	if err := json.Unmarshal(entry.Payload, &payload); err != nil {
		return redisstore.ErrIntegrity
	}
	// A user deleted since the settlement keeps no link in the projection.
	payload.Reservation.Subject = payload.Subject
	if payload.Subject == "" {
		payload.Reservation.Anonymous = false
		kept := payload.Reservation.Items[:0:0]
		settled := []int64{}
		for i, item := range payload.Reservation.Items {
			if item.Scope == "app" {
				kept = append(kept, item)
				settled = append(settled, payload.Settlement.Settled[i])
			}
		}
		payload.Reservation.Items, payload.Settlement.Settled = kept, settled
	}
	call, cancel := context.WithTimeout(ctx, redisCall)
	defer cancel()
	status, err := c.o.Redis.Settle(call, redisstore.Ending{Reservation: payload.Reservation, Settlement: payload.Settlement, Digest: payload.Digest})
	if err != nil {
		return err
	}
	if status == redisstore.SettleUnknown {
		// The epoch never saw this admission: only a rebuild from the ledger
		// can account for it correctly.
		c.o.Recover("unknown settlement")
		return redisstore.ErrFenced
	}
	return nil
}

// RetryPending retries the settlements and slot releases that failed. It is
// called by the worker; a success removes the item.
func (c *Coordinator) RetryPending(ctx context.Context) error {
	c.mu.Lock()
	retry, slots := c.retry, c.slots
	c.retry, c.slots = nil, nil
	c.mu.Unlock()
	var failed []*ending
	var failure error
	for _, e := range retry {
		e.a.mu.Lock()
		if e.a.ended {
			e.a.mu.Unlock()
			continue
		}
		err := c.persistEnding(ctx, e.a, e.st, nil)
		if err == nil {
			e.a.ended = true
		}
		e.a.mu.Unlock()
		if err != nil {
			failed = append(failed, e)
			failure = errors.Join(failure, err)
		}
	}
	var failedSlots []slotRelease
	for _, s := range slots {
		call, cancel := context.WithTimeout(ctx, redisCall)
		err := c.o.Redis.ReleaseSlot(call, s.owner, s.app, s.subject, s.request)
		cancel()
		if err != nil {
			failedSlots = append(failedSlots, s)
		}
	}
	c.mu.Lock()
	c.retry = append(c.retry, failed...)
	c.slots = append(c.slots, failedSlots...)
	c.mu.Unlock()
	return failure
}

// PendingSettlements reports obligations waiting for a retry, for the deletion
// of a user and for readiness.
func (c *Coordinator) PendingSettlements(subjects []storage.SubjectRef) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.retry {
		for _, s := range subjects {
			if e.a.res.AppName == s.App && e.a.res.Subject == s.Hash {
				return true
			}
		}
	}
	return false
}

// Backlog is the number of local retries, for readiness and metrics.
func (c *Coordinator) Backlog() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.retry) + len(c.slots)
}

// sortedApps is used by the workers to process apps in a stable order.
func sortedApps(apps map[string]bool) []string {
	out := make([]string, 0, len(apps))
	for app := range apps {
		out = append(out, app)
	}
	slices.Sort(out)
	return out
}
