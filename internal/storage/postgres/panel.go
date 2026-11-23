package postgres

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/events"
	"github.com/JoaoVictorVM/comporta/internal/storage"
)

var _ storage.PanelStore = (*Store)(nil)

// panelTx is a repeatable read, so a list and its totals come from the same
// state on any replica.
func (s *Store) panelTx(ctx context.Context) (*sql.Tx, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return nil, storage.ErrUnavailable
	}
	return tx, nil
}

// PanelTopUsers ranks the users of an app by cost in a range of day labels.
func (s *Store) PanelTopUsers(ctx context.Context, app, from, through string) ([]storage.PanelUserCost, error) {
	if !storage.RecordName.MatchString(app) {
		return nil, storage.ErrInvalid
	}
	tx, err := s.panelTx(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT subject,anonymous,sum(cost_microusd)::bigint,sum(requests)::bigint FROM usage_daily WHERE scope='user' AND app_name=$1 AND day>=$2 AND day<=$3 GROUP BY subject,anonymous ORDER BY sum(cost_microusd) DESC,subject,anonymous LIMIT $4`, app, from, through, storage.PanelTopUsers)
	if err != nil {
		return nil, storage.ErrUnavailable
	}
	defer rows.Close()
	var out []storage.PanelUserCost
	for rows.Next() {
		var u storage.PanelUserCost
		var cost int64
		if err = rows.Scan(&u.Hash, &u.Anonymous, &cost, &u.Requests); err != nil {
			return nil, storage.ErrUnavailable
		}
		u.Cost = configMicrousd(cost)
		out = append(out, u)
	}
	if rows.Err() != nil {
		return nil, storage.ErrUnavailable
	}
	return out, nil
}

// PanelModelMix counts attempts and cost by the model actually used.
func (s *Store) PanelModelMix(ctx context.Context, app string, from, until time.Time) ([]storage.PanelModelUse, error) {
	if !storage.RecordName.MatchString(app) {
		return nil, storage.ErrInvalid
	}
	tx, err := s.panelTx(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT coalesce(model_used,''),count(*),sum(cost_microusd)::bigint FROM requests WHERE app_name=$1 AND attempt_index>=1 AND started_at>=$2 AND started_at<$3 GROUP BY coalesce(model_used,'') ORDER BY count(*) DESC,coalesce(model_used,'') LIMIT $4`, app, micros(from), micros(until), storage.PanelMaxModels)
	if err != nil {
		return nil, storage.ErrUnavailable
	}
	defer rows.Close()
	var out []storage.PanelModelUse
	for rows.Next() {
		var m storage.PanelModelUse
		var cost int64
		if err = rows.Scan(&m.Model, &m.Attempts, &cost); err != nil {
			return nil, storage.ErrUnavailable
		}
		m.Cost = configMicrousd(cost)
		out = append(out, m)
	}
	if rows.Err() != nil {
		return nil, storage.ErrUnavailable
	}
	return out, nil
}

// PanelRequests lists attempts newest first, after the cursor of the previous
// page.
func (s *Store) PanelRequests(ctx context.Context, f storage.PanelRequestFilter) (storage.PanelRequestPage, error) {
	var page storage.PanelRequestPage
	limit, err := storage.ValidatePanelRequestFilter(f)
	if err != nil {
		return page, err
	}
	before := storage.RecordCursor{StartedAt: time.UnixMicro(1<<63 - 1)}
	if f.Before != nil {
		before = *f.Before
	}
	tx, err := s.panelTx(ctx)
	if err != nil {
		return page, err
	}
	defer tx.Rollback()
	var clauses []string
	var args []any
	next := func() string { return "$" + strconv.Itoa(len(args)) }
	if f.App != "" {
		args = append(args, f.App)
		clauses = append(clauses, `app_name=`+next())
	}
	if f.UserHash != "" {
		args = append(args, f.UserHash)
		hash := next()
		args = append(args, f.Anonymous)
		clauses = append(clauses, `user_hash=`+hash+` AND anonymous=`+next())
	}
	if f.Model != "" {
		args = append(args, f.Model)
		clauses = append(clauses, `model_used=`+next())
	}
	if f.Outcome != "" {
		args = append(args, f.Outcome)
		clauses = append(clauses, `outcome=`+next())
	}
	args = append(args, micros(before.StartedAt))
	at := next()
	args = append(args, before.RequestID)
	id := next()
	args = append(args, before.AttemptIndex)
	clauses = append(clauses, `(started_at,request_id,attempt_index)<(`+at+`,`+id+`,`+next()+`)`)
	args = append(args, limit+1)
	rows, err := tx.QueryContext(ctx, `SELECT record_json::text FROM requests WHERE `+strings.Join(clauses, ` AND `)+` ORDER BY started_at DESC,request_id DESC,attempt_index DESC LIMIT `+next(), args...)
	if err != nil {
		return page, storage.ErrUnavailable
	}
	defer rows.Close()
	for rows.Next() {
		var data string
		if err = rows.Scan(&data); err != nil {
			return page, storage.ErrUnavailable
		}
		if len(page.Records) == limit {
			last := page.Records[len(page.Records)-1]
			page.Next = &storage.RecordCursor{StartedAt: last.StartedAt, RequestID: last.RequestID, AttemptIndex: last.AttemptIndex}
			break
		}
		_, record, err := canonicalRecord(data)
		if err != nil {
			return page, err
		}
		page.Records = append(page.Records, record)
	}
	if rows.Err() != nil {
		return page, storage.ErrUnavailable
	}
	return page, nil
}

// PanelRequest reads one attempt.
func (s *Store) PanelRequest(ctx context.Context, requestID string, attempt int) (storage.Record, bool, error) {
	var record storage.Record
	if !storage.HexID.MatchString(requestID) || attempt < 0 || attempt > storage.MaxAttempts {
		return record, false, storage.ErrInvalid
	}
	tx, err := s.panelTx(ctx)
	if err != nil {
		return record, false, err
	}
	defer tx.Rollback()
	var data string
	err = tx.QueryRowContext(ctx, `SELECT record_json::text FROM requests WHERE request_id=$1 AND attempt_index=$2`, requestID, attempt).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return record, false, nil
	}
	if err != nil {
		return record, false, storage.ErrUnavailable
	}
	_, record, err = canonicalRecord(data)
	if err != nil {
		return record, false, err
	}
	return record, true, nil
}

// PanelEvents lists events newest first.
func (s *Store) PanelEvents(ctx context.Context, f storage.PanelEventFilter) (events.Page, error) {
	var page events.Page
	limit, err := storage.ValidatePanelEventFilter(f)
	if err != nil {
		return page, err
	}
	beforeTime, beforeID := int64(1<<63-1), ""
	if f.Before != nil {
		beforeTime, beforeID = micros(f.Before.At), f.Before.ID
	}
	query := `SELECT event_id,type,occurred_at,coalesce(request_id,''),coalesce(app_name,''),coalesce(user_hash,''),anonymous,payload_json::text FROM events WHERE ($1='' OR app_name=$1) AND (occurred_at,event_id)<($2,$3)`
	args := []any{f.App, beforeTime, beforeID}
	if len(f.Types) > 0 {
		var marks []string
		for _, kind := range f.Types {
			args = append(args, string(kind))
			marks = append(marks, "$"+strconv.Itoa(len(args)))
		}
		query += ` AND type IN (` + strings.Join(marks, ",") + `)`
	}
	args = append(args, limit+1)
	query += ` ORDER BY occurred_at DESC,event_id DESC LIMIT $` + strconv.Itoa(len(args))
	tx, err := s.panelTx(ctx)
	if err != nil {
		return page, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return page, storage.ErrUnavailable
	}
	defer rows.Close()
	for rows.Next() {
		var row storage.EventRow
		var anonymous sql.NullBool
		if err = rows.Scan(&row.ID, &row.Type, &row.At, &row.RequestID, &row.App, &row.Hash, &anonymous, &row.Payload); err != nil {
			return page, storage.ErrUnavailable
		}
		if anonymous.Valid {
			row.Anonymous = &anonymous.Bool
		}
		if len(page.Events) == limit {
			last := page.Events[len(page.Events)-1]
			page.Next = &events.Cursor{At: last.At, ID: last.ID}
			break
		}
		e, err := storage.DecodeEventRow(row)
		if err != nil {
			return page, err
		}
		page.Events = append(page.Events, e)
	}
	if rows.Err() != nil {
		return page, storage.ErrUnavailable
	}
	return page, nil
}

const panelSentAttempts = `FROM requests WHERE started_at>=$1 AND started_at<$2 AND attempt_index>=1 AND coalesce(provider,'')<>'' AND outcome NOT IN ('cached','blocked','loop_cut')`

const panelFailure = `coalesce(error_class,'') IN ('overload','timeout','connection','upstream_5xx')`

// PanelProviders aggregates the attempts of a window per provider, with the
// same nearest-rank percentiles as the SQLite backend.
func (s *Store) PanelProviders(ctx context.Context, from, until time.Time) ([]storage.PanelProviderStats, []storage.PanelProviderBucket, error) {
	tx, err := s.panelTx(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	start, end := micros(from), micros(until)
	rows, err := tx.QueryContext(ctx, `WITH sent AS (SELECT provider,latency_us,error_class,input_tokens,cache_read_tokens `+panelSentAttempts+`),
ranked AS (SELECT provider,latency_us,row_number() OVER (PARTITION BY provider ORDER BY latency_us) AS rn,count(*) OVER (PARTITION BY provider) AS n FROM sent),
pct AS (SELECT provider,min(CASE WHEN rn>=(n*50+99)/100 THEN latency_us END) AS p50,min(CASE WHEN rn>=(n*95+99)/100 THEN latency_us END) AS p95 FROM ranked GROUP BY provider)
SELECT sent.provider,count(*),sum(CASE WHEN coalesce(error_class,'')='' THEN 1 ELSE 0 END),sum(CASE WHEN `+panelFailure+` THEN 1 ELSE 0 END),pct.p50,pct.p95,coalesce(sum(input_tokens),0)::bigint,coalesce(sum(cache_read_tokens),0)::bigint
FROM sent JOIN pct ON pct.provider=sent.provider GROUP BY sent.provider,pct.p50,pct.p95 ORDER BY sent.provider`, start, end)
	if err != nil {
		return nil, nil, storage.ErrUnavailable
	}
	var stats []storage.PanelProviderStats
	for rows.Next() {
		var p storage.PanelProviderStats
		if err = rows.Scan(&p.Provider, &p.Attempts, &p.Successes, &p.Failures, &p.P50LatencyUS, &p.P95LatencyUS, &p.InputTokens, &p.CacheReadToks); err != nil {
			rows.Close()
			return nil, nil, storage.ErrUnavailable
		}
		stats = append(stats, p)
	}
	failed := rows.Err() != nil
	rows.Close()
	if failed {
		return nil, nil, storage.ErrUnavailable
	}
	rows, err = tx.QueryContext(ctx, `SELECT provider,((started_at-$1)/$3)::integer AS idx,count(*),sum(CASE WHEN `+panelFailure+` THEN 1 ELSE 0 END) `+panelSentAttempts+` GROUP BY provider,idx ORDER BY provider,idx`, start, end, storage.PanelBucket.Microseconds())
	if err != nil {
		return nil, nil, storage.ErrUnavailable
	}
	defer rows.Close()
	var buckets []storage.PanelProviderBucket
	for rows.Next() {
		var b storage.PanelProviderBucket
		if err = rows.Scan(&b.Provider, &b.Index, &b.Attempts, &b.Failures); err != nil {
			return nil, nil, storage.ErrUnavailable
		}
		buckets = append(buckets, b)
	}
	if rows.Err() != nil {
		return nil, nil, storage.ErrUnavailable
	}
	return stats, buckets, nil
}

// PanelWatermark reads the newest request and event and how many landed
// recently. Every replica sees the writes of the others here.
func (s *Store) PanelWatermark(ctx context.Context) (storage.PanelWatermark, error) {
	var w storage.PanelWatermark
	tx, err := s.panelTx(ctx)
	if err != nil {
		return w, err
	}
	defer tx.Rollback()
	since := time.Now().Add(-storage.WatermarkSpan).UnixMicro()
	err = tx.QueryRowContext(ctx, `SELECT coalesce((SELECT max(finished_at) FROM requests),0),(SELECT count(*) FROM requests WHERE finished_at>=$1),coalesce((SELECT max(occurred_at) FROM events),0),(SELECT count(*) FROM events WHERE occurred_at>=$1)`, since).
		Scan(&w.RequestsLatest, &w.RequestsRecent, &w.EventsLatest, &w.EventsRecent)
	if err != nil {
		return w, storage.ErrUnavailable
	}
	return w, nil
}
