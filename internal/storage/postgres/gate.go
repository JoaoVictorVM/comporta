package postgres

import (
	"context"
	"database/sql"
	"errors"
)

// GateView is what an admission checks under the shared gate, read in one
// round trip together with the app and operation locks.
type GateView struct {
	Epoch          string
	Mode           string
	ConfigRevision int64
	LeaseValid     bool
	GlobalPaused   bool
	AppPaused      bool
	PendingOutbox  bool
	Maintenance    bool
}

// AdmissionGate takes the app lock, then the operation lock, and reads the
// cluster record, the lease, the pause overrides and whether the app has
// settlements not yet projected. The caller already holds the shared gate.
func AdmissionGate(ctx context.Context, tx *sql.Tx, cluster, app, request, subject string, lease Lease) (GateView, error) {
	var v GateView
	var appLock, opLock string
	err := tx.QueryRowContext(ctx, `SELECT c.epoch,c.mode,c.config_revision,
pg_advisory_xact_lock($2)::text,
pg_advisory_xact_lock($3)::text,
EXISTS(SELECT 1 FROM replica_leases WHERE instance_id=$4 AND fence=$5 AND state='active' AND lease_until>(extract(epoch from clock_timestamp())*1000000)::bigint),
coalesce((SELECT (value_json->>'paused')::boolean FROM runtime_state WHERE namespace='pause_global' AND key='state'),false),
EXISTS(SELECT 1 FROM runtime_state WHERE namespace='pause_app' AND key=$6 AND (value_json->>'paused')::boolean),
EXISTS(SELECT 1 FROM coordination_outbox WHERE app_name=$6 AND delivered_at IS NULL),
EXISTS(SELECT 1 FROM deletion_jobs WHERE phase<>'done' AND subjects @> jsonb_build_array(jsonb_build_object('app',$6::text,'hash',$7::text)))
FROM cluster_state c WHERE c.cluster_id=$1`, cluster, AppKey(cluster, app), OperationKey(cluster, request), lease.InstanceID, lease.Fence, app, subject).
		Scan(&v.Epoch, &v.Mode, &v.ConfigRevision, &appLock, &opLock, &v.LeaseValid, &v.GlobalPaused, &v.AppPaused, &v.PendingOutbox, &v.Maintenance)
	if errors.Is(err, sql.ErrNoRows) {
		return v, ErrNoCluster
	}
	if err != nil {
		return v, databaseError(err)
	}
	return v, nil
}
