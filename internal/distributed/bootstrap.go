package distributed

import (
	"context"
	"errors"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/storage/postgres"
	redisstore "github.com/JoaoVictorVM/comporta/internal/storage/redis"
)

// Identity is what every replica of a cluster must agree on before it admits.
type Identity struct {
	Revision            int64
	ConfigFingerprint   string
	IdentityFingerprint string
}

// Failures of a start, with fixed operator messages.
var (
	ErrIdentityMismatch = errors.New("shared user identity key differs from the one this cluster was created with; mount the cluster's secret")
	ErrRevisionConflict = errors.New("deployment.config_revision is already in use with a different configuration; raise the revision to roll out a change")
	ErrRevisionBehind   = errors.New("this replica's deployment.config_revision is older than the cluster's; it stays not ready until it gets the configuration in force")
	ErrNamespaceInUse   = errors.New("redis already holds keys for this cluster but the shared database has no cluster record; use empty stores for a new cluster")
	ErrRecoveryNeeded   = errors.New("shared limit state must be rebuilt before admissions")
)

// Bootstrap prepares the cluster under the exclusive gate. A new cluster gets
// its first epoch only in empty stores. An existing one is checked against
// this process's identity and revision: a higher revision is activated, a
// lower one leaves the replica not ready, an equal one must match exactly.
func Bootstrap(ctx context.Context, sql *postgres.Store, redis *redisstore.Store, cluster string, id Identity) (postgres.ClusterState, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	tx, err := sql.DB().BeginTx(ctx, nil)
	if err != nil {
		return postgres.ClusterState{}, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, postgres.GateKey(cluster)); err != nil {
		return postgres.ClusterState{}, err
	}
	run, err := redis.RunID(ctx)
	if err != nil {
		return postgres.ClusterState{}, err
	}
	state, err := postgres.LoadCluster(ctx, tx, cluster)
	switch {
	case errors.Is(err, postgres.ErrNoCluster):
		state = postgres.ClusterState{ClusterID: cluster, Epoch: postgres.NewEpoch(), Mode: postgres.ModeNormal, ConfigRevision: id.Revision,
			ConfigFingerprint: id.ConfigFingerprint, IdentityFingerprint: id.IdentityFingerprint, RedisRunID: run}
		empty, err := redis.Empty(ctx, "comporta:"+cluster+":")
		if err != nil {
			return state, err
		}
		if !empty {
			return state, ErrNamespaceInUse
		}
		if err := postgres.BootstrapCluster(ctx, tx, state); err != nil {
			return state, err
		}
		redis.UseEpoch(cluster, state.Epoch)
		if err := redis.Publish(ctx, state.ConfigRevision); err != nil {
			return state, err
		}
		if err := tx.Commit(); err != nil {
			return state, err
		}
		redis.Expect(run)
		return state, nil
	case err != nil:
		return state, err
	}
	if state.IdentityFingerprint != id.IdentityFingerprint {
		return state, ErrIdentityMismatch
	}
	switch {
	case id.Revision < state.ConfigRevision:
		return state, ErrRevisionBehind
	case id.Revision == state.ConfigRevision && id.ConfigFingerprint != state.ConfigFingerprint:
		return state, ErrRevisionConflict
	case id.Revision > state.ConfigRevision:
		state.ConfigRevision, state.ConfigFingerprint = id.Revision, id.ConfigFingerprint
		if err := postgres.UpdateCluster(ctx, tx, state); err != nil {
			return state, err
		}
	}
	redis.UseEpoch(cluster, state.Epoch)
	if state.RedisRunID != run || state.Mode == postgres.ModeRecovering {
		if err := tx.Commit(); err != nil {
			return state, err
		}
		return state, ErrRecoveryNeeded
	}
	if err := tx.Commit(); err != nil {
		return state, err
	}
	redis.Expect(run)
	return state, nil
}
