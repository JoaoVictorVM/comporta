package postgres

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash/fnv"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/storage"
)

// Cluster modes.
const (
	ModeNormal     = "normal"
	ModeDegraded   = "degraded"
	ModeRecovering = "recovering"
)

// ClusterState is the shared record of one cluster.
type ClusterState struct {
	ClusterID           string
	Epoch               string
	Mode                string
	ConfigRevision      int64
	ConfigFingerprint   string
	IdentityFingerprint string
	RedisRunID          string
	ControlRevision     int64
	DegradedSince       time.Time
}

// ErrClusterMismatch means the database belongs to another cluster, or the
// shared identity or configuration of this process differs from it.
var ErrClusterMismatch = errors.New("shared database belongs to another cluster or identity")

// ErrNoCluster means the cluster was never bootstrapped in this database.
var ErrNoCluster = errors.New("cluster not bootstrapped")

// NewEpoch draws an opaque epoch identifier.
func NewEpoch() string {
	var raw [16]byte
	_, _ = rand.Read(raw[:])
	return hex.EncodeToString(raw[:])
}

// GateKey is the advisory key of a cluster's control gate, derived from its
// identifier. A collision only serializes two clusters; it never mixes data.
func GateKey(cluster string) int64 {
	h := fnv.New64a()
	h.Write([]byte("gate:" + cluster))
	return int64(h.Sum64())
}

// AppKey is the advisory key that orders the financial transactions of one app.
func AppKey(cluster, app string) int64 {
	h := fnv.New64a()
	h.Write([]byte("app:" + cluster + "\x00" + app))
	return int64(h.Sum64())
}

// OperationKey is the advisory key of one request, held by its admission until
// commit, so a reconciler can prove no creating transaction is still open.
func OperationKey(cluster, requestID string) int64 {
	h := fnv.New64a()
	h.Write([]byte("op:" + cluster + "\x00" + requestID))
	return int64(h.Sum64())
}

type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// LoadCluster reads the cluster record.
func LoadCluster(ctx context.Context, q rowQuerier, cluster string) (ClusterState, error) {
	var c ClusterState
	var run sql.NullString
	var degraded sql.NullInt64
	err := q.QueryRowContext(ctx, `SELECT cluster_id,epoch,mode,config_revision,config_fingerprint,identity_fingerprint,redis_run_id,control_revision,degraded_since FROM cluster_state WHERE cluster_id=$1`, cluster).
		Scan(&c.ClusterID, &c.Epoch, &c.Mode, &c.ConfigRevision, &c.ConfigFingerprint, &c.IdentityFingerprint, &run, &c.ControlRevision, &degraded)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNoCluster
	}
	if err != nil {
		return c, storage.ErrUnavailable
	}
	c.RedisRunID = run.String
	if degraded.Valid {
		c.DegradedSince = time.UnixMicro(degraded.Int64).UTC()
	}
	return c, nil
}

// ClusterStatus is the non-secret summary printed by "storage status".
type ClusterStatus struct {
	Mode           string
	ConfigRevision int64
	ActiveReplicas int64
	PendingOutbox  int64
}

func (s *Store) ClusterStatus(ctx context.Context, cluster string) (ClusterStatus, error) {
	var out ClusterStatus
	c, err := LoadCluster(ctx, s.db, cluster)
	if err != nil {
		return out, err
	}
	out.Mode, out.ConfigRevision = c.Mode, c.ConfigRevision
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM replica_leases WHERE state='active' AND lease_until>$1`, nowMicros()).Scan(&out.ActiveReplicas); err != nil {
		return out, storage.ErrUnavailable
	}
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM coordination_outbox WHERE delivered_at IS NULL`).Scan(&out.PendingOutbox); err != nil {
		return out, storage.ErrUnavailable
	}
	return out, nil
}

// BootstrapCluster creates the cluster record in an empty database. Another
// cluster already present refuses: one database belongs to one cluster.
func BootstrapCluster(ctx context.Context, tx *sql.Tx, c ClusterState) error {
	var others int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM cluster_state WHERE cluster_id<>$1`, c.ClusterID).Scan(&others); err != nil {
		return databaseError(err)
	}
	if others > 0 {
		return ErrClusterMismatch
	}
	var history int
	if err := tx.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM metering_journal)+(SELECT count(*) FROM limit_ledger)+(SELECT count(*) FROM limit_reservations)`).Scan(&history); err != nil {
		return databaseError(err)
	}
	if history > 0 {
		return ErrClusterMismatch
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO cluster_state(cluster_id,epoch,mode,config_revision,config_fingerprint,identity_fingerprint,redis_run_id,control_revision,updated_at) VALUES($1,$2,$3,$4,$5,$6,NULLIF($7,''),0,$8)`,
		c.ClusterID, c.Epoch, c.Mode, c.ConfigRevision, c.ConfigFingerprint, c.IdentityFingerprint, c.RedisRunID, nowMicros()); err != nil {
		return databaseError(err)
	}
	return nil
}

// UpdateCluster writes the mutable fields of the cluster record. The caller
// holds the exclusive gate.
func UpdateCluster(ctx context.Context, tx *sql.Tx, c ClusterState) error {
	var degraded any
	if !c.DegradedSince.IsZero() {
		degraded = micros(c.DegradedSince)
	}
	result, err := tx.ExecContext(ctx, `UPDATE cluster_state SET epoch=$2,mode=$3,config_revision=$4,config_fingerprint=$5,redis_run_id=NULLIF($6,''),degraded_since=$7,updated_at=$8 WHERE cluster_id=$1`,
		c.ClusterID, c.Epoch, c.Mode, c.ConfigRevision, c.ConfigFingerprint, c.RedisRunID, degraded, nowMicros())
	if err != nil {
		return databaseError(err)
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return ErrNoCluster
	}
	return nil
}

// DatabaseNow is the clock every lease is compared with, so replica clocks
// never decide ownership.
func DatabaseNow(ctx context.Context, q rowQuerier) (int64, error) {
	var now int64
	if err := q.QueryRowContext(ctx, `SELECT (extract(epoch from clock_timestamp())*1000000)::bigint`).Scan(&now); err != nil {
		return 0, storage.ErrUnavailable
	}
	return now, nil
}

// Lease is the right of one process to admit.
type Lease struct {
	InstanceID string
	Fence      int64
	Until      int64
}

// AcquireLease registers a new process with a fresh fencing token.
func (s *Store) AcquireLease(ctx context.Context, instance string, revision int64, ttl time.Duration) (Lease, error) {
	l := Lease{InstanceID: instance}
	err := s.db.QueryRowContext(ctx, `INSERT INTO replica_leases(instance_id,fence,lease_until,config_revision,state)
VALUES($1,nextval('replica_fence'),(extract(epoch from clock_timestamp())*1000000)::bigint+$2,$3,'active') RETURNING fence,lease_until`,
		instance, ttl.Microseconds(), revision).Scan(&l.Fence, &l.Until)
	if err != nil {
		return l, databaseError(err)
	}
	return l, nil
}

// ErrLeaseLost means the lease expired or was fenced by a takeover.
var ErrLeaseLost = errors.New("replica lease lost")

// RenewLease extends an active lease that has not expired. Once expired it can
// never be renewed: the process must stop admitting and start over.
func (s *Store) RenewLease(ctx context.Context, l Lease, revision int64, ttl time.Duration, ackRevision int64) (Lease, error) {
	var until int64
	err := s.db.QueryRowContext(ctx, `UPDATE replica_leases SET lease_until=(extract(epoch from clock_timestamp())*1000000)::bigint+$3,config_revision=$4,cache_ack_revision=greatest(cache_ack_revision,$5)
WHERE instance_id=$1 AND fence=$2 AND state IN ('active','draining') AND lease_until>(extract(epoch from clock_timestamp())*1000000)::bigint RETURNING lease_until`,
		l.InstanceID, l.Fence, ttl.Microseconds(), revision, ackRevision).Scan(&until)
	if errors.Is(err, sql.ErrNoRows) {
		return l, ErrLeaseLost
	}
	if err != nil {
		return l, storage.ErrUnavailable
	}
	l.Until = until
	return l, nil
}

// MarkDraining keeps the lease while the process finishes what it admitted.
func (s *Store) MarkDraining(ctx context.Context, l Lease) error {
	_, err := s.db.ExecContext(ctx, `UPDATE replica_leases SET state='draining' WHERE instance_id=$1 AND fence=$2 AND state='active'`, l.InstanceID, l.Fence)
	return err
}

// ReleaseLease ends the lease of a process that stopped cleanly after its
// journal was flushed; a recovery then has nothing to settle.
func (s *Store) ReleaseLease(ctx context.Context, l Lease) error {
	_, err := s.db.ExecContext(ctx, `UPDATE replica_leases SET state='fenced',lease_until=0 WHERE instance_id=$1 AND fence=$2`, l.InstanceID, l.Fence)
	return err
}

// LeaseValid checks the lease inside a transaction, against the database clock.
func LeaseValid(ctx context.Context, tx *sql.Tx, l Lease) (bool, error) {
	var ok bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM replica_leases WHERE instance_id=$1 AND fence=$2 AND state='active' AND lease_until>(extract(epoch from clock_timestamp())*1000000)::bigint)`, l.InstanceID, l.Fence).Scan(&ok)
	if err != nil {
		return false, databaseError(err)
	}
	return ok, nil
}

// ExpiredLeases lists the processes whose lease ran out without a release.
func (s *Store) ExpiredLeases(ctx context.Context) ([]Lease, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT instance_id,fence,lease_until FROM replica_leases WHERE state<>'fenced' AND lease_until<=(extract(epoch from clock_timestamp())*1000000)::bigint ORDER BY lease_until LIMIT 100`)
	if err != nil {
		return nil, storage.ErrUnavailable
	}
	defer rows.Close()
	var out []Lease
	for rows.Next() {
		var l Lease
		if err := rows.Scan(&l.InstanceID, &l.Fence, &l.Until); err != nil {
			return nil, storage.ErrUnavailable
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// FenceLease marks an expired lease as taken over, by compare-and-set on its
// fencing token. Only one reconciler wins.
func FenceLease(ctx context.Context, tx *sql.Tx, l Lease) (bool, error) {
	result, err := tx.ExecContext(ctx, `UPDATE replica_leases SET state='fenced' WHERE instance_id=$1 AND fence=$2 AND state<>'fenced' AND lease_until<=(extract(epoch from clock_timestamp())*1000000)::bigint`, l.InstanceID, l.Fence)
	if err != nil {
		return false, databaseError(err)
	}
	n, _ := result.RowsAffected()
	return n == 1, nil
}

// PurgeFenced deletes lease rows fenced long ago. Operations keep their owner
// identifier without a foreign key, so history survives.
func (s *Store) PurgeFenced(ctx context.Context, before time.Time) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM replica_leases WHERE state='fenced' AND lease_until<$1 AND NOT EXISTS(SELECT 1 FROM metering_journal j WHERE j.owner_id=replica_leases.instance_id)`, micros(before))
	return err
}

// LiveOwners lists the processes whose lease is still valid.
func LiveOwners(ctx context.Context, q queryer) (map[string]bool, error) {
	rows, err := q.QueryContext(ctx, `SELECT instance_id FROM replica_leases WHERE state IN ('active','draining') AND lease_until>(extract(epoch from clock_timestamp())*1000000)::bigint`)
	if err != nil {
		return nil, storage.ErrUnavailable
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, storage.ErrUnavailable
		}
		out[id] = true
	}
	return out, rows.Err()
}

// OutboxEntry is one transition of the shared projection that must be applied
// after its SQL commit.
type OutboxEntry struct {
	ID          int64
	OperationID string
	Transition  string
	AppName     string
	Payload     json.RawMessage
}

// PutOutbox records a transition in the caller's transaction.
func PutOutbox(ctx context.Context, tx *sql.Tx, operation, transition, app string, payload any) (int64, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return 0, storage.ErrInvalid
	}
	var id int64
	err = tx.QueryRowContext(ctx, `INSERT INTO coordination_outbox(operation_id,transition,app_name,payload,created_at) VALUES($1,$2,$3,$4,$5)
ON CONFLICT(operation_id,transition) DO UPDATE SET operation_id=excluded.operation_id RETURNING id`, operation, transition, app, string(data), nowMicros()).Scan(&id)
	if err != nil {
		return 0, databaseError(err)
	}
	return id, nil
}

// MarkDelivered acknowledges a transition applied to the projection.
func (s *Store) MarkDelivered(ctx context.Context, ids ...int64) error {
	for _, id := range ids {
		if _, err := s.db.ExecContext(ctx, `UPDATE coordination_outbox SET delivered_at=$1,claim_token=NULL WHERE id=$2 AND delivered_at IS NULL`, nowMicros(), id); err != nil {
			return storage.ErrUnavailable
		}
	}
	return nil
}

// ClaimOutbox takes up to limit undelivered transitions for 30 seconds. A
// claim that expires lets another worker retry; it never means the first one
// failed, so every transition is applied idempotently.
func (s *Store) ClaimOutbox(ctx context.Context, app string, limit int) ([]OutboxEntry, error) {
	token := NewEpoch()
	now := nowMicros()
	rows, err := s.db.QueryContext(ctx, `UPDATE coordination_outbox SET claimed_until=$1,claim_token=$2 WHERE id IN (
SELECT id FROM coordination_outbox WHERE delivered_at IS NULL AND (claimed_until IS NULL OR claimed_until<$3) AND ($4='' OR app_name=$4)
ORDER BY id LIMIT $5 FOR UPDATE SKIP LOCKED) RETURNING id,operation_id,transition,app_name,payload::text`,
		now+(30*time.Second).Microseconds(), token, now, app, limit)
	if err != nil {
		return nil, storage.ErrUnavailable
	}
	defer rows.Close()
	var out []OutboxEntry
	for rows.Next() {
		var e OutboxEntry
		var payload string
		if err := rows.Scan(&e.ID, &e.OperationID, &e.Transition, &e.AppName, &payload); err != nil {
			return nil, storage.ErrUnavailable
		}
		e.Payload = json.RawMessage(payload)
		out = append(out, e)
	}
	return out, rows.Err()
}

// PendingOutboxFor lists the undelivered transitions of one app, in order,
// without claiming them; an admission replays them under the app lock.
func PendingOutboxFor(ctx context.Context, tx *sql.Tx, app string, limit int) ([]OutboxEntry, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id,operation_id,transition,app_name,payload::text FROM coordination_outbox WHERE delivered_at IS NULL AND app_name=$1 ORDER BY id LIMIT $2`, app, limit)
	if err != nil {
		return nil, databaseError(err)
	}
	defer rows.Close()
	var out []OutboxEntry
	for rows.Next() {
		var e OutboxEntry
		var payload string
		if err := rows.Scan(&e.ID, &e.OperationID, &e.Transition, &e.AppName, &payload); err != nil {
			return nil, databaseError(err)
		}
		e.Payload = json.RawMessage(payload)
		out = append(out, e)
	}
	return out, rows.Err()
}

// OldestPending is the age of the oldest undelivered transition, for metrics.
func (s *Store) OldestPending(ctx context.Context) (time.Duration, int64, error) {
	var oldest sql.NullInt64
	var count int64
	if err := s.db.QueryRowContext(ctx, `SELECT min(created_at),count(*) FROM coordination_outbox WHERE delivered_at IS NULL`).Scan(&oldest, &count); err != nil {
		return 0, 0, storage.ErrUnavailable
	}
	if !oldest.Valid {
		return 0, 0, nil
	}
	return time.Duration(nowMicros()-oldest.Int64) * time.Microsecond, count, nil
}

// PruneOutbox removes transitions delivered before the cutoff.
func (s *Store) PruneOutbox(ctx context.Context, before time.Time) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM coordination_outbox WHERE delivered_at IS NOT NULL AND delivered_at<$1`, micros(before))
	return err
}

// CacheEpochs reads the invalidation revision of every app.
func (s *Store) CacheEpochs(ctx context.Context) (map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT app_name,revision FROM cache_epochs`)
	if err != nil {
		return nil, storage.ErrUnavailable
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var app string
		var revision int64
		if err := rows.Scan(&app, &revision); err != nil {
			return nil, storage.ErrUnavailable
		}
		out[app] = revision
	}
	return out, rows.Err()
}

// BumpCacheEpochs invalidates the caches of apps on every replica.
func BumpCacheEpochs(ctx context.Context, tx *sql.Tx, apps []string) error {
	for _, app := range apps {
		if !storage.RecordName.MatchString(app) {
			return storage.ErrInvalid
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO cache_epochs VALUES($1,1,$2) ON CONFLICT(app_name) DO UPDATE SET revision=cache_epochs.revision+1,updated_at=excluded.updated_at`, app, nowMicros()); err != nil {
			return databaseError(err)
		}
	}
	return nil
}
