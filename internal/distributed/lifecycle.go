package distributed

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/events"
	"github.com/JoaoVictorVM/comporta/internal/storage/postgres"
	redisstore "github.com/JoaoVictorVM/comporta/internal/storage/redis"
)

// Lease timing: a heartbeat every 2 seconds keeps a 15 second lease, renewed
// on the database clock. A process that cannot renew stops admitting when its
// own conservative estimate of the lease ends.
const (
	LeaseTTL      = 15 * time.Second
	Heartbeat     = 2 * time.Second
	controlPoll   = 500 * time.Millisecond
	checkpointGap = 5 * time.Second
	leaseMargin   = 3 * time.Second
)

// ErrLeaseLost stops the process: another replica may already have recovered
// what it admitted, so it must not keep serving.
var ErrLeaseLost = postgres.ErrLeaseLost

// LifecycleOptions configures the background work of one replica.
type LifecycleOptions struct {
	Coordinator *Coordinator
	SQL         *postgres.Store
	Redis       *redisstore.Store
	Cluster     string
	// Identity is the revision and fingerprints of the configuration in force.
	Identity func() Identity
	// Quarantine is how long loop history lost with the projection could have
	// mattered: the longest similar window, cooldown or minute.
	Quarantine func() time.Duration
	// OnControl refreshes local state after a pause or a deletion raised the
	// control revision: pauses, cache invalidation and the usage queue. The
	// revision is acknowledged only after it succeeds.
	OnControl func(ctx context.Context, revision int64) error
	Logger    *slog.Logger
	Publisher events.Publisher
}

// Lifecycle owns the lease of this process and every reconciliation the
// cluster needs: takeovers, orphans, checkpoints, rebuilds and deletions.
type Lifecycle struct {
	o         LifecycleOptions
	lease     atomic.Pointer[postgres.Lease]
	validTill atomic.Int64
	ready     atomic.Bool
	acked     atomic.Int64
	draining  atomic.Bool
	recover   chan string
	history   *History

	mu       sync.Mutex
	revision int64
}

// Start acquires a new lease with a fresh fencing token. An identifier is
// never reused, so a restarted process is always a new owner.
func Start(ctx context.Context, o LifecycleOptions) (*Lifecycle, error) {
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.Quarantine == nil {
		o.Quarantine = func() time.Duration { return time.Minute }
	}
	l := &Lifecycle{o: o, recover: make(chan string, 1), history: o.Coordinator.History()}
	id := o.Identity()
	lease, err := o.SQL.AcquireLease(ctx, postgres.NewEpoch(), id.Revision, LeaseTTL)
	if err != nil {
		return nil, err
	}
	l.setLease(lease)
	o.SQL.SetOwner(postgres.Owner{ID: lease.InstanceID, Fence: lease.Fence})
	l.revision = id.Revision
	l.ready.Store(true)
	return l, nil
}

func (l *Lifecycle) setLease(lease postgres.Lease) {
	l.lease.Store(&lease)
	l.validTill.Store(time.Now().Add(LeaseTTL - leaseMargin).UnixNano())
}

// Lease is the lease to admit under, while it is surely still valid.
func (l *Lifecycle) Lease() (postgres.Lease, bool) {
	lease := l.lease.Load()
	if lease == nil || time.Now().UnixNano() >= l.validTill.Load() {
		return postgres.Lease{}, false
	}
	return *lease, true
}

// Signal asks for a rebuild or an epoch refresh; repeated signals coalesce.
func (l *Lifecycle) Signal(reason string) {
	select {
	case l.recover <- reason:
	default:
	}
}

// Ready reports whether this replica may take traffic: a valid lease, the
// configuration revision in force and a cluster that is not rebuilding.
func (l *Lifecycle) Ready() bool {
	if l.draining.Load() || !l.ready.Load() {
		return false
	}
	if _, ok := l.Lease(); !ok {
		return false
	}
	return l.o.Coordinator.Mode() != postgres.ModeRecovering
}

// Drain marks the replica not ready before it stops admitting.
func (l *Lifecycle) Drain() { l.draining.Store(true) }

// Run performs the periodic work until ctx ends. Losing the lease ends it
// with an error, which stops the process.
func (l *Lifecycle) Run(ctx context.Context) error {
	beat := time.NewTicker(Heartbeat)
	defer beat.Stop()
	control := time.NewTicker(controlPoll)
	defer control.Stop()
	checkpoint := time.NewTicker(checkpointGap)
	defer checkpoint.Stop()
	for {
		select {
		case <-ctx.Done():
			l.stop()
			return nil
		case reason := <-l.recover:
			l.reconcile(ctx, reason)
		case <-control.C:
			l.control(ctx)
		case <-checkpoint.C:
			if err := l.o.Coordinator.Checkpoint(ctx); err != nil {
				l.o.Logger.Warn("limit checkpoint postponed")
			}
		case <-beat.C:
			if err := l.heartbeat(ctx); err != nil {
				return err
			}
		}
	}
}

func (l *Lifecycle) heartbeat(ctx context.Context) error {
	lease := l.lease.Load()
	renewed, err := l.o.SQL.RenewLease(ctx, *lease, l.currentRevision(), LeaseTTL, l.acked.Load())
	switch {
	case errors.Is(err, postgres.ErrLeaseLost):
		l.ready.Store(false)
		l.o.Logger.Error("replica lease lost; stopping so its work is recovered by another replica")
		return ErrLeaseLost
	case err != nil:
		l.o.Logger.Warn("replica lease not renewed; admissions stop if it cannot be renewed in time")
		return nil
	}
	l.setLease(renewed)
	c := l.o.Coordinator
	if c.o.Redis.ServerChanged() {
		l.reconcile(ctx, "redis server changed")
	} else if c.Mode() == postgres.ModeDegraded {
		ping, cancel := context.WithTimeout(ctx, time.Second)
		_, err := c.o.Redis.RunID(ping)
		cancel()
		if err == nil {
			l.reconcile(ctx, "redis back")
		}
	}
	l.checkRevision(ctx)
	expired, err := l.o.SQL.ExpiredLeases(ctx)
	if err == nil {
		for _, dead := range expired {
			if err := c.Takeover(ctx, dead); err != nil {
				l.o.Logger.Warn("takeover of an expired replica postponed")
			}
		}
	}
	if c.Mode() == postgres.ModeNormal {
		_ = c.ReconcileOrphans(ctx)
		_ = c.ProjectOutbox(ctx)
	}
	_ = c.RetryPending(ctx)
	_ = l.history.ResumeDeletions(ctx)
	_ = l.o.SQL.PruneOutbox(ctx, time.Now().Add(-time.Hour))
	_ = l.o.SQL.PurgeFenced(ctx, time.Now().Add(-24*time.Hour))
	return nil
}

// reconcile follows a new epoch when the server is the recorded one, and
// rebuilds under the exclusive gate otherwise.
func (l *Lifecycle) reconcile(ctx context.Context, reason string) {
	c := l.o.Coordinator
	rebuild, err := c.RefreshEpoch(ctx)
	if err != nil {
		return
	}
	if !rebuild && reason != "projection" && reason != "unknown settlement" && reason != "redis back" && reason != "redis server changed" {
		return
	}
	l.o.Logger.Warn("rebuilding shared limit state", "reason", reason)
	if err := c.Rebuild(ctx, l.o.Quarantine()); err != nil && !errors.Is(err, ErrRebuildBusy) {
		l.o.Logger.Error("shared limit state rebuild failed; admissions stay refused")
	}
}

func (l *Lifecycle) currentRevision() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.revision
}

// checkRevision activates a higher configuration revision under the
// exclusive gate, and keeps a replica with an older or conflicting one not
// ready until it gets the configuration in force.
func (l *Lifecycle) checkRevision(ctx context.Context) {
	id := l.o.Identity()
	state, err := postgres.LoadCluster(ctx, l.o.SQL.DB(), l.o.Cluster)
	if err != nil {
		return
	}
	if id.Revision == state.ConfigRevision && id.ConfigFingerprint == state.ConfigFingerprint {
		l.ready.Store(true)
		l.mu.Lock()
		l.revision = id.Revision
		l.mu.Unlock()
		return
	}
	_, err = Bootstrap(ctx, l.o.SQL, l.o.Redis, l.o.Cluster, id)
	switch {
	case err == nil, errors.Is(err, ErrRecoveryNeeded):
		l.mu.Lock()
		l.revision = id.Revision
		l.mu.Unlock()
		l.ready.Store(true)
		if err != nil {
			l.Signal("redis server changed")
		}
	default:
		l.ready.Store(false)
		l.o.Logger.Warn("replica not ready: configuration revision differs from the cluster's")
	}
}

// control refreshes local projections when the control revision moved, then
// acknowledges it so a deletion waiting on every replica can proceed.
func (l *Lifecycle) control(ctx context.Context) {
	revision, err := l.o.Coordinator.ControlRevision(ctx)
	if err != nil || revision <= l.acked.Load() {
		return
	}
	if l.o.OnControl != nil {
		if err := l.o.OnControl(ctx, revision); err != nil {
			return
		}
	}
	l.acked.Store(revision)
	lease := l.lease.Load()
	if renewed, err := l.o.SQL.RenewLease(ctx, *lease, l.currentRevision(), LeaseTTL, revision); err == nil {
		l.setLease(renewed)
	}
}

// stop releases the lease only when nothing of this process is left in the
// journal; otherwise the lease expires and another replica recovers it.
func (l *Lifecycle) stop() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c := l.o.Coordinator
	_ = c.RetryPending(ctx)
	lease := l.lease.Load()
	pending, err := l.o.SQL.PendingOf(ctx, lease.InstanceID)
	if err != nil || len(pending) > 0 || c.Backlog() > 0 {
		return
	}
	var open int
	if err := l.o.SQL.DB().QueryRowContext(ctx, `SELECT count(*) FROM limit_reservations WHERE owner_id=$1 AND state='reserved'`, lease.InstanceID).Scan(&open); err != nil || open > 0 {
		return
	}
	_ = c.o.Redis.ReleaseOwner(ctx, lease.InstanceID)
	_ = l.o.SQL.ReleaseLease(ctx, *lease)
}
