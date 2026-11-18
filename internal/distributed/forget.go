package distributed

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/storage"
	"github.com/JoaoVictorVM/comporta/internal/storage/postgres"
	redisstore "github.com/JoaoVictorVM/comporta/internal/storage/redis"
)

// History is the deletion of users across replicas. It is a durable job:
//
//  1. under the exclusive gate, the job is published as a barrier that every
//     admission checks, the caches of the apps are invalidated and the
//     control revision is raised;
//  2. outside any lock, it waits until every live replica acknowledged that
//     revision (caches purged, usage queue flushed) and no obligation of the
//     users is left in Postgres;
//  3. under the exclusive gate, the history is deleted and the job committed;
//  4. the projection is cleaned and the job ends with a receipt that names
//     no one.
//
// Giving up before step 3 deletes nothing; a failure after it is resumed by
// the lifecycle worker and reported as pending, never as "nothing removed".
type History struct {
	c       *Coordinator
	prepare time.Duration
}

// History returns the deletion store of the admin service.
func (c *Coordinator) History() *History { return &History{c: c, prepare: 10 * time.Second} }

var _ storage.ForgetStore = (*History)(nil)

func (h *History) HistoricalApps(ctx context.Context) ([]string, error) {
	return h.c.o.SQL.HistoricalApps(ctx)
}

type jobSubject struct {
	App  string `json:"app"`
	Hash string `json:"hash"`
}

func (h *History) ForgetSubjects(ctx context.Context, subjects []storage.SubjectRef) (storage.ForgetCounts, error) {
	for _, s := range subjects {
		if !storage.RecordName.MatchString(s.App) || !storage.HexID.MatchString(s.Hash) {
			return storage.ForgetCounts{}, storage.ErrInvalid
		}
	}
	job := postgres.NewEpoch()
	revision, err := h.publish(ctx, job, subjects)
	if err != nil {
		return storage.ForgetCounts{}, err
	}
	deadline := time.Now().Add(h.prepare)
	for {
		ready, err := h.drained(ctx, revision, subjects)
		if err == nil && ready {
			break
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			h.abandon(job)
			return storage.ForgetCounts{}, storage.BusyError{Reason: "replicas still hold usage of the user"}
		}
		time.Sleep(100 * time.Millisecond)
	}
	counts, err := h.commit(ctx, job, subjects)
	if err != nil {
		h.abandon(job)
		return storage.ForgetCounts{}, err
	}
	if err := h.clean(ctx, job, subjects, counts); err != nil {
		return counts, storage.ErrPending
	}
	return counts, nil
}

func (h *History) publish(ctx context.Context, job string, subjects []storage.SubjectRef) (int64, error) {
	tx, err := exclusiveGate(ctx, h.c.o.SQL.DB(), h.c.o.Cluster, 4*time.Second)
	if err != nil {
		return 0, storage.ErrUnavailable
	}
	defer tx.Rollback()
	var list []jobSubject
	var apps []string
	for _, s := range subjects {
		list = append(list, jobSubject{App: s.App, Hash: s.Hash})
		apps = append(apps, s.App)
	}
	var revision int64
	if err := tx.QueryRowContext(ctx, `UPDATE cluster_state SET control_revision=control_revision+1 WHERE cluster_id=$1 RETURNING control_revision`, h.c.o.Cluster).Scan(&revision); err != nil {
		return 0, storage.ErrUnavailable
	}
	subjectsJSON, _ := json.Marshal(list)
	appsJSON, _ := json.Marshal(apps)
	now := time.Now().UTC().UnixMicro()
	if _, err := tx.ExecContext(ctx, `INSERT INTO deletion_jobs VALUES($1,'preparing',$2,$3,$4,$5,$5,NULL)`, job, revision, string(subjectsJSON), string(appsJSON), now); err != nil {
		return 0, storage.ErrUnavailable
	}
	if err := postgres.BumpCacheEpochs(ctx, tx, apps); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, storage.ErrUnavailable
	}
	return revision, nil
}

// drained reports whether every live replica acknowledged the revision and no
// obligation of the users is still open anywhere.
func (h *History) drained(ctx context.Context, revision int64, subjects []storage.SubjectRef) (bool, error) {
	var lagging int
	if err := h.c.o.SQL.DB().QueryRowContext(ctx, `SELECT count(*) FROM replica_leases WHERE state IN ('active','draining') AND lease_until>(extract(epoch from clock_timestamp())*1000000)::bigint AND cache_ack_revision<$1`, revision).Scan(&lagging); err != nil {
		return false, err
	}
	if lagging > 0 {
		return false, nil
	}
	for _, s := range subjects {
		var open bool
		err := h.c.o.SQL.DB().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM limit_reservations WHERE app_name=$1 AND subject=$2 AND NOT anonymous AND state='reserved')
OR EXISTS(SELECT 1 FROM request_operations WHERE app_name=$1 AND subject=$2 AND NOT anonymous AND state='active')
OR EXISTS(SELECT 1 FROM metering_journal j WHERE j.metadata_json->>'AppName'=$1 AND j.metadata_json->>'UserHash'=$2 AND (j.metadata_json->>'Anonymous')::boolean=false
  AND NOT (j.state='completed' AND EXISTS(SELECT 1 FROM requests r WHERE r.request_id=j.request_id AND r.attempt_index=j.attempt_index)))`, s.App, s.Hash).Scan(&open)
		if err != nil {
			return false, err
		}
		if open {
			return false, nil
		}
	}
	return !h.c.PendingSettlements(subjects), nil
}

func (h *History) commit(ctx context.Context, job string, subjects []storage.SubjectRef) (storage.ForgetCounts, error) {
	tx, err := exclusiveGate(ctx, h.c.o.SQL.DB(), h.c.o.Cluster, 4*time.Second)
	if err != nil {
		return storage.ForgetCounts{}, storage.ErrUnavailable
	}
	defer tx.Rollback()
	counts, err := h.c.o.SQL.ForgetSubjects(postgres.WithScope(ctx, &postgres.Scope{Tx: tx}), subjects)
	if err != nil {
		return counts, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE deletion_jobs SET phase='committed',updated_at=$2 WHERE job_id=$1`, job, time.Now().UTC().UnixMicro()); err != nil {
		return counts, storage.ErrUnavailable
	}
	if err := tx.Commit(); err != nil {
		// Ambiguous: the job row tells the lifecycle worker which way it went.
		return counts, storage.ErrPending
	}
	return counts, nil
}

// clean removes the users from the projection and closes the job. The
// receipt keeps counts only; the pseudonyms are erased with it.
func (h *History) clean(ctx context.Context, job string, subjects []storage.SubjectRef, counts storage.ForgetCounts) error {
	for _, s := range subjects {
		call, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := h.c.o.Redis.ForgetSubject(call, s.App, redisstore.SubjectKey(s.Hash, false))
		cancel()
		if err != nil {
			return err
		}
	}
	receipt, _ := json.Marshal(struct {
		Requests int64 `json:"requests"`
		Events   int64 `json:"events"`
	}{counts.Requests, counts.Events})
	_, err := h.c.o.SQL.DB().ExecContext(ctx, `UPDATE deletion_jobs SET phase='done',subjects=NULL,receipt=$2,updated_at=$3 WHERE job_id=$1`, job, string(receipt), time.Now().UTC().UnixMicro())
	return err
}

// abandon removes a job that never deleted anything, lifting the barrier.
func (h *History) abandon(job string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = h.c.o.SQL.DB().ExecContext(ctx, `DELETE FROM deletion_jobs WHERE job_id=$1 AND phase='preparing'`, job)
}

// ResumeDeletions finishes jobs whose history was deleted but whose
// projection was not cleaned, and drops jobs abandoned while preparing.
func (h *History) ResumeDeletions(ctx context.Context) error {
	rows, err := h.c.o.SQL.DB().QueryContext(ctx, `SELECT job_id,phase,subjects::text FROM deletion_jobs WHERE phase IN ('preparing','committed','cleaning') AND updated_at<$1`,
		time.Now().Add(-30*time.Second).UTC().UnixMicro())
	if err != nil {
		return storage.ErrUnavailable
	}
	type pending struct {
		job, phase string
		subjects   []storage.SubjectRef
	}
	var jobs []pending
	for rows.Next() {
		var p pending
		var data string
		if err := rows.Scan(&p.job, &p.phase, &data); err != nil {
			rows.Close()
			return storage.ErrUnavailable
		}
		var list []jobSubject
		if json.Unmarshal([]byte(data), &list) != nil {
			continue
		}
		for _, s := range list {
			p.subjects = append(p.subjects, storage.SubjectRef{App: s.App, Hash: s.Hash})
		}
		jobs = append(jobs, p)
	}
	rows.Close()
	var failure error
	for _, p := range jobs {
		if p.phase == "preparing" {
			h.abandon(p.job)
			continue
		}
		failure = errors.Join(failure, h.clean(ctx, p.job, p.subjects, storage.ForgetCounts{}))
	}
	return failure
}
