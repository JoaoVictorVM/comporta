package sqlite

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/events"
	"github.com/JoaoVictorVM/comporta/internal/storage"
)

var _ storage.PanelStore = (*Store)(nil)

func (s *Store) panelTx(ctx context.Context) (*sql.Tx, error) {
	db := s.readDB
	if db == nil {
		db = s.db
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, storage.ErrUnavailable
	}
	return tx, nil
}

// PanelTopUsers ranks the users of an app by cost in a range of day labels.
// Ties are broken by the pseudonym, so the ranking is stable.
func (s *Store) PanelTopUsers(ctx context.Context, app, from, through string) ([]storage.PanelUserCost, error) {
	if !storage.RecordName.MatchString(app) {
		return nil, storage.ErrInvalid
	}
	tx, err := s.panelTx(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT subject,anonymous,sum(cost_microusd),sum(requests) FROM usage_daily WHERE scope='user' AND app_name=? AND day>=? AND day<=? GROUP BY subject,anonymous ORDER BY sum(cost_microusd) DESC,subject,anonymous LIMIT ?`, app, from, through, storage.PanelTopUsers)
	if err != nil {
		return nil, storage.ErrUnavailable
	}
	defer rows.Close()
	var out []storage.PanelUserCost
	for rows.Next() {
		var u storage.PanelUserCost
		if err = rows.Scan(&u.Hash, &u.Anonymous, &u.Cost, &u.Requests); err != nil {
			return nil, storage.ErrUnavailable
		}
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
	rows, err := tx.QueryContext(ctx, `SELECT coalesce(model_used,''),count(*),sum(cost_microusd) FROM requests WHERE app_name=? AND attempt_index>=1 AND started_at>=? AND started_at<? GROUP BY coalesce(model_used,'') ORDER BY count(*) DESC,coalesce(model_used,'') LIMIT ?`, app, from.UnixMicro(), until.UnixMicro(), storage.PanelMaxModels)
	if err != nil {
		return nil, storage.ErrUnavailable
	}
	defer rows.Close()
	var out []storage.PanelModelUse
	for rows.Next() {
		var m storage.PanelModelUse
		if err = rows.Scan(&m.Model, &m.Attempts, &m.Cost); err != nil {
			return nil, storage.ErrUnavailable
		}
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
	query, args := panelRequestsQuery(f, before, limit)
	rows, err := tx.QueryContext(ctx, query, args...)
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
		var record storage.Record
		if err = decodeRecord(data, &record); err != nil {
			return page, err
		}
		page.Records = append(page.Records, record)
	}
	if rows.Err() != nil {
		return page, storage.ErrUnavailable
	}
	return page, nil
}

// panelRequestsQuery writes only the clauses of the filters present, so each
// filter can use its own index in descending order.
func panelRequestsQuery(f storage.PanelRequestFilter, before storage.RecordCursor, limit int) (string, []any) {
	var clauses []string
	var args []any
	if f.App != "" {
		clauses, args = append(clauses, `app_name=?`), append(args, f.App)
	}
	if f.UserHash != "" {
		clauses, args = append(clauses, `user_hash=? AND anonymous=?`), append(args, f.UserHash, f.Anonymous)
	}
	if f.Model != "" {
		clauses, args = append(clauses, `model_used=?`), append(args, f.Model)
	}
	if f.Outcome != "" {
		clauses, args = append(clauses, `outcome=?`), append(args, f.Outcome)
	}
	clauses, args = append(clauses, `(started_at,request_id,attempt_index)<(?,?,?)`), append(args, before.StartedAt.UnixMicro(), before.RequestID, before.AttemptIndex)
	args = append(args, limit+1)
	return `SELECT record_json FROM requests WHERE ` + strings.Join(clauses, ` AND `) + ` ORDER BY started_at DESC,request_id DESC,attempt_index DESC LIMIT ?`, args
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
	err = tx.QueryRowContext(ctx, `SELECT record_json FROM requests WHERE request_id=? AND attempt_index=?`, requestID, attempt).Scan(&data)
	if err == sql.ErrNoRows {
		return record, false, nil
	}
	if err != nil {
		return record, false, storage.ErrUnavailable
	}
	if err = decodeRecord(data, &record); err != nil {
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
		beforeTime, beforeID = f.Before.At.UnixMicro(), f.Before.ID
	}
	query := `SELECT event_id,type,occurred_at,coalesce(request_id,''),coalesce(app_name,''),coalesce(user_hash,''),anonymous,payload_json FROM events WHERE (?='' OR app_name=?) AND (occurred_at,event_id)<(?,?)`
	args := []any{f.App, f.App, beforeTime, beforeID}
	if len(f.Types) > 0 {
		query += ` AND type IN (` + strings.TrimSuffix(strings.Repeat("?,", len(f.Types)), ",") + `)`
		for _, kind := range f.Types {
			args = append(args, string(kind))
		}
	}
	query += ` ORDER BY occurred_at DESC,event_id DESC LIMIT ?`
	args = append(args, limit+1)
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

// panelSentAttempts is the set of attempts that reached a provider: refusals
// and cache hits never did.
const panelSentAttempts = `FROM requests WHERE started_at>=? AND started_at<? AND attempt_index>=1 AND coalesce(provider,'')<>'' AND outcome NOT IN ('cached','blocked','loop_cut')`

const panelFailure = `coalesce(error_class,'') IN ('overload','timeout','connection','upstream_5xx')`

// PanelProviders aggregates the attempts of a window per provider. Percentiles
// use the nearest rank and are computed by the database, so the attempts of
// the hour are never loaded into memory.
func (s *Store) PanelProviders(ctx context.Context, from, until time.Time) ([]storage.PanelProviderStats, []storage.PanelProviderBucket, error) {
	tx, err := s.panelTx(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	start, end := from.UnixMicro(), until.UnixMicro()
	rows, err := tx.QueryContext(ctx, `WITH sent AS (SELECT provider,latency_us,error_class,input_tokens,cache_read_tokens `+panelSentAttempts+`),
ranked AS (SELECT provider,latency_us,row_number() OVER (PARTITION BY provider ORDER BY latency_us) AS rn,count(*) OVER (PARTITION BY provider) AS n FROM sent),
pct AS (SELECT provider,min(CASE WHEN rn>=(n*50+99)/100 THEN latency_us END) AS p50,min(CASE WHEN rn>=(n*95+99)/100 THEN latency_us END) AS p95 FROM ranked GROUP BY provider)
SELECT sent.provider,count(*),sum(CASE WHEN coalesce(error_class,'')='' THEN 1 ELSE 0 END),sum(CASE WHEN `+panelFailure+` THEN 1 ELSE 0 END),pct.p50,pct.p95,coalesce(sum(input_tokens),0),coalesce(sum(cache_read_tokens),0)
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
	bucket := storage.PanelBucket.Microseconds()
	rows, err = tx.QueryContext(ctx, `SELECT provider,(started_at-?)/?,count(*),sum(CASE WHEN `+panelFailure+` THEN 1 ELSE 0 END) `+panelSentAttempts+` GROUP BY provider,(started_at-?)/? ORDER BY provider,2`, start, bucket, start, end, start, bucket)
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
// recently, in one read.
func (s *Store) PanelWatermark(ctx context.Context) (storage.PanelWatermark, error) {
	var w storage.PanelWatermark
	tx, err := s.panelTx(ctx)
	if err != nil {
		return w, err
	}
	defer tx.Rollback()
	since := time.Now().Add(-storage.WatermarkSpan).UnixMicro()
	err = tx.QueryRowContext(ctx, `SELECT coalesce((SELECT max(finished_at) FROM requests),0),(SELECT count(*) FROM requests WHERE finished_at>=?),coalesce((SELECT max(occurred_at) FROM events),0),(SELECT count(*) FROM events WHERE occurred_at>=?)`, since, since).
		Scan(&w.RequestsLatest, &w.RequestsRecent, &w.EventsLatest, &w.EventsRecent)
	if err != nil {
		return w, storage.ErrUnavailable
	}
	return w, nil
}
