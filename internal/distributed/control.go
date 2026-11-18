package distributed

import (
	"context"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/storage"
	"github.com/JoaoVictorVM/comporta/internal/storage/postgres"
)

// PauseStore makes an operator pause durable under the exclusive gate. When
// SavePause returns, no admission on any replica can commit without seeing
// it, because every admission reads the override in its shared gate. The
// Redis projection is updated afterwards and only accelerates refusals: a
// projection that missed the change never authorizes a paused app.
type PauseStore struct{ c *Coordinator }

// Pauses returns the store the runtime control uses in distributed mode.
func (c *Coordinator) Pauses() *PauseStore { return &PauseStore{c: c} }

var _ storage.PauseStore = (*PauseStore)(nil)

func (p *PauseStore) LoadPause(ctx context.Context) (storage.PauseState, error) {
	return p.c.o.SQL.LoadPause(ctx)
}

func (p *PauseStore) SavePause(ctx context.Context, change storage.PauseChange) (storage.PauseState, bool, error) {
	call, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, err := exclusiveGate(call, p.c.o.SQL.DB(), p.c.o.Cluster, 4*time.Second)
	if err != nil {
		return storage.PauseState{}, false, storage.ErrUnavailable
	}
	defer tx.Rollback()
	scope := &postgres.Scope{Tx: tx}
	state, changed, err := p.c.o.SQL.SavePause(postgres.WithScope(call, scope), change)
	if err != nil {
		return storage.PauseState{}, false, err
	}
	cluster, err := postgres.LoadCluster(call, tx, p.c.o.Cluster)
	if err != nil {
		return storage.PauseState{}, false, err
	}
	if err := tx.Commit(); err != nil {
		// The commit may have happened: the caller must read the state again
		// instead of assuming nothing changed.
		return storage.PauseState{}, false, storage.ErrUnavailable
	}
	scope.Committed()
	p.c.projectPause(state, cluster.ControlRevision)
	return state, changed, nil
}

func (c *Coordinator) projectPause(state storage.PauseState, revision int64) {
	var apps []string
	for _, a := range state.Apps {
		if a.Paused {
			apps = append(apps, a.App)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), redisCall)
	defer cancel()
	if err := c.o.Redis.SetPause(ctx, revision, state.Global, apps); err != nil {
		c.logger.Warn("pause projection delayed; the durable state still applies")
	}
}

// ControlRevision reads the revision every pause change raises, so replicas
// refresh their local projection only when something changed.
func (c *Coordinator) ControlRevision(ctx context.Context) (int64, error) {
	var revision int64
	err := c.o.SQL.DB().QueryRowContext(ctx, `SELECT control_revision FROM cluster_state WHERE cluster_id=$1`, c.o.Cluster).Scan(&revision)
	if err != nil {
		return 0, storage.ErrUnavailable
	}
	return revision, nil
}
