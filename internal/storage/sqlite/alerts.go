package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/events"
	"github.com/JoaoVictorVM/comporta/internal/storage"
)

// The alert inbox and the five minute aggregate are filled by triggers of the
// 0006 migration, in the same transaction as the event or the request row, so
// no write path can skip them.

func (s *Store) ClaimAlertInputs(ctx context.Context, now time.Time, lease time.Duration, limit int) ([]storage.AlertInput, error) {
	if limit < 1 || limit > storage.MaxAlertBatch || lease <= 0 {
		return nil, storage.ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, databaseError(err)
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT event_id,event_type,enqueued_at FROM alert_inputs WHERE state='pending' OR (state='claimed' AND claimed_until<=?) ORDER BY enqueued_at,event_id LIMIT ?`, now.UnixMicro(), limit)
	if err != nil {
		return nil, databaseError(err)
	}
	var out []storage.AlertInput
	for rows.Next() {
		var in storage.AlertInput
		var at int64
		if err = rows.Scan(&in.EventID, &in.EventType, &at); err != nil {
			rows.Close()
			return nil, databaseError(err)
		}
		in.EnqueuedAt = time.UnixMicro(at).UTC()
		in.ClaimToken = storage.NewClaimToken()
		out = append(out, in)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, databaseError(err)
	}
	rows.Close()
	until := now.Add(lease).UnixMicro()
	for i := range out {
		if _, err = tx.ExecContext(ctx, `UPDATE alert_inputs SET state='claimed',claim_token=?,claimed_until=? WHERE event_id=?`, out[i].ClaimToken, until, out[i].EventID); err != nil {
			return nil, databaseError(err)
		}
		if out[i].Event, err = loadEvent(ctx, tx, out[i].EventID); err != nil {
			return nil, err
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, databaseError(err)
	}
	return out, nil
}

func loadEvent(ctx context.Context, tx *sql.Tx, id string) (*events.Event, error) {
	var e events.Event
	var micros int64
	var payload, hash string
	var anonymous sql.NullBool
	err := tx.QueryRowContext(ctx, `SELECT event_id,type,occurred_at,coalesce(request_id,''),coalesce(app_name,''),coalesce(user_hash,''),anonymous,payload_json FROM events WHERE event_id=?`, id).
		Scan(&e.ID, &e.Type, &micros, &e.RequestID, &e.App, &hash, &anonymous, &payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, databaseError(err)
	}
	if (e.Type == events.LoopCut || e.Type == events.SensitiveDataDetected) && hash != "" && anonymous.Valid {
		e.UserHash = hash
		flag := anonymous.Bool
		e.Anonymous = &flag
	}
	e.At = time.UnixMicro(micros).UTC()
	if err = json.Unmarshal([]byte(payload), &e.Payload); err != nil || e.Validate() != nil {
		return nil, storage.ErrInvalid
	}
	return &e, nil
}

func (s *Store) CompleteAlertInput(ctx context.Context, eventID, token string, occurrences []storage.AlertOccurrence, channels []storage.AlertChannelRef, now time.Time) error {
	if err := validateAlertWrite(occurrences, channels); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return databaseError(err)
	}
	defer tx.Rollback()
	var state string
	var current sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT state,claim_token FROM alert_inputs WHERE event_id=?`, eventID).Scan(&state, &current)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && (state != "claimed" || current.String != token)) {
		return storage.ErrClaimLost
	}
	if err != nil {
		return databaseError(err)
	}
	if err = insertOccurrences(ctx, tx, occurrences, channels, now); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE alert_inputs SET state='processed',processed_at=?,claim_token=NULL,claimed_until=NULL WHERE event_id=?`, now.UnixMicro(), eventID); err != nil {
		return databaseError(err)
	}
	if err = tx.Commit(); err != nil {
		return databaseError(err)
	}
	return nil
}

func (s *Store) ReleaseAlertInput(ctx context.Context, eventID, token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.ExecContext(ctx, `UPDATE alert_inputs SET state='pending',claim_token=NULL,claimed_until=NULL WHERE event_id=? AND state='claimed' AND claim_token=?`, eventID, token); err != nil {
		return databaseError(err)
	}
	return nil
}

func (s *Store) RecordAlertOccurrences(ctx context.Context, occurrences []storage.AlertOccurrence, channels []storage.AlertChannelRef, now time.Time) error {
	if err := validateAlertWrite(occurrences, channels); err != nil {
		return err
	}
	if len(occurrences) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return databaseError(err)
	}
	defer tx.Rollback()
	if err = insertOccurrences(ctx, tx, occurrences, channels, now); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return databaseError(err)
	}
	return nil
}

func validateAlertWrite(occurrences []storage.AlertOccurrence, channels []storage.AlertChannelRef) error {
	for _, o := range occurrences {
		if err := storage.ValidateOccurrence(o); err != nil {
			return err
		}
	}
	for _, c := range channels {
		if !storage.RecordName.MatchString(c.Name) || !storage.ValidChannelType(c.Type) {
			return storage.ErrInvalid
		}
	}
	return nil
}

// insertOccurrences writes the occurrences whose key is new and expands their
// deliveries. A known key is a confirmation: no channel is added afterwards.
func insertOccurrences(ctx context.Context, tx *sql.Tx, occurrences []storage.AlertOccurrence, channels []storage.AlertChannelRef, now time.Time) error {
	if len(occurrences) == 0 {
		return nil
	}
	if len(channels) > 0 {
		var open int64
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM alert_deliveries WHERE state IN('pending','claimed')`).Scan(&open); err != nil {
			return databaseError(err)
		}
		if open+int64(len(occurrences)*len(channels)) > storage.MaxAlertDeliveries {
			return storage.ErrAlertBacklogFull
		}
	}
	at := now.UnixMicro()
	for _, o := range occurrences {
		var anonymous any
		if o.Anonymous != nil {
			anonymous = *o.Anonymous
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO alert_occurrences(occurrence_id,dedupe_key,trigger,occurred_at,app_name,scope,user_hash,anonymous,dimension,window_start,window_end,threshold_percent,payload_json,created_at)
VALUES(?,?,?,?,nullif(?,''),?,nullif(?,''),?,nullif(?,''),?,?,nullif(?,0),?,?) ON CONFLICT DO NOTHING`,
			o.ID, o.DedupeKey, o.Trigger, o.OccurredAt.UnixMicro(), o.App, o.Scope, o.UserHash, anonymous, o.Dimension, optionalMicros(o.WindowStart), optionalMicros(o.WindowEnd), o.ThresholdPercent, string(o.Payload), at)
		if err != nil {
			return databaseError(err)
		}
		if n, err := result.RowsAffected(); err != nil || n == 0 {
			continue
		}
		for _, c := range channels {
			if _, err = tx.ExecContext(ctx, `INSERT INTO alert_deliveries(delivery_id,occurrence_id,channel_name,channel_type,state,attempt_count,next_attempt_at,created_at) VALUES(?,?,?,?,'pending',0,?,?) ON CONFLICT DO NOTHING`,
				storage.DeliveryID(o.ID, c.Name), o.ID, c.Name, c.Type, at, at); err != nil {
				return databaseError(err)
			}
		}
	}
	return nil
}

func optionalMicros(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UnixMicro()
}

const occurrenceColumns = `o.occurrence_id,o.dedupe_key,o.trigger,o.occurred_at,coalesce(o.app_name,''),o.scope,coalesce(o.user_hash,''),o.anonymous,coalesce(o.dimension,''),o.window_start,o.window_end,coalesce(o.threshold_percent,0),o.payload_json,o.created_at`

type scanner interface{ Scan(...any) error }

func scanOccurrence(row scanner, prefix ...any) (storage.AlertOccurrence, error) {
	var o storage.AlertOccurrence
	var occurred, created int64
	var anonymous sql.NullBool
	var start, end sql.NullInt64
	var payload string
	dest := append(prefix, &o.ID, &o.DedupeKey, &o.Trigger, &occurred, &o.App, &o.Scope, &o.UserHash, &anonymous, &o.Dimension, &start, &end, &o.ThresholdPercent, &payload, &created)
	if err := row.Scan(dest...); err != nil {
		return o, err
	}
	o.OccurredAt, o.CreatedAt, o.Payload = time.UnixMicro(occurred).UTC(), time.UnixMicro(created).UTC(), []byte(payload)
	if anonymous.Valid {
		flag := anonymous.Bool
		o.Anonymous = &flag
	}
	if start.Valid {
		t := time.UnixMicro(start.Int64).UTC()
		o.WindowStart = &t
	}
	if end.Valid {
		t := time.UnixMicro(end.Int64).UTC()
		o.WindowEnd = &t
	}
	return o, nil
}

// ClaimAlertDeliveries takes, for each channel without a delivery in flight,
// its oldest due delivery. One delivery per channel keeps the order of each
// receiver and never bursts it.
func (s *Store) ClaimAlertDeliveries(ctx context.Context, now time.Time, lease time.Duration, limit int) ([]storage.AlertDelivery, error) {
	if limit < 1 || limit > storage.MaxAlertBatch || lease <= 0 {
		return nil, storage.ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, databaseError(err)
	}
	defer tx.Rollback()
	at := now.UnixMicro()
	busy := map[string]bool{}
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT channel_name FROM alert_deliveries WHERE state='claimed' AND claimed_until>?`, at)
	if err != nil {
		return nil, databaseError(err)
	}
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			rows.Close()
			return nil, databaseError(err)
		}
		busy[name] = true
	}
	rows.Close()
	rows, err = tx.QueryContext(ctx, `SELECT d.delivery_id,d.channel_name,d.channel_type,d.state,d.attempt_count,d.created_at,`+occurrenceColumns+`
FROM alert_deliveries d JOIN alert_occurrences o ON o.occurrence_id=d.occurrence_id
WHERE (d.state='pending' OR (d.state='claimed' AND d.claimed_until<=?)) AND d.next_attempt_at<=?
ORDER BY d.created_at,d.delivery_id LIMIT 1000`, at, at)
	if err != nil {
		return nil, databaseError(err)
	}
	var out []storage.AlertDelivery
	for rows.Next() && len(out) < limit {
		var d storage.AlertDelivery
		var created int64
		o, err := scanOccurrence(rows, &d.ID, &d.Channel, &d.ChannelType, &d.State, &d.Attempts, &created)
		if err != nil {
			rows.Close()
			return nil, databaseError(err)
		}
		if busy[d.Channel] {
			continue
		}
		busy[d.Channel] = true
		d.CreatedAt, d.Occurrence, d.OccurrenceID = time.UnixMicro(created).UTC(), o, o.ID
		d.ClaimToken = storage.NewClaimToken()
		out = append(out, d)
	}
	rows.Close()
	until := now.Add(lease).UnixMicro()
	for i := range out {
		if _, err = tx.ExecContext(ctx, `UPDATE alert_deliveries SET state='claimed',claim_token=?,claimed_until=? WHERE delivery_id=?`, out[i].ClaimToken, until, out[i].ID); err != nil {
			return nil, databaseError(err)
		}
		out[i].State = storage.DeliveryClaimed
	}
	if err = tx.Commit(); err != nil {
		return nil, databaseError(err)
	}
	return out, nil
}

func (s *Store) FinishAlertDelivery(ctx context.Context, deliveryID, token string, result storage.AlertResult, now time.Time) error {
	if err := storage.ValidateAlertResult(result); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return databaseError(err)
	}
	defer tx.Rollback()
	var state string
	var current sql.NullString
	var attempts int
	err = tx.QueryRowContext(ctx, `SELECT state,claim_token,attempt_count FROM alert_deliveries WHERE delivery_id=?`, deliveryID).Scan(&state, &current, &attempts)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && (state != storage.DeliveryClaimed || current.String != token)) {
		return storage.ErrClaimLost
	}
	if err != nil {
		return databaseError(err)
	}
	next, count := storage.NextDeliveryState(attempts, result)
	at := now.UnixMicro()
	nextAttempt := at
	var delivered, finished any
	switch next {
	case storage.DeliveryPending:
		nextAttempt = result.RetryAt.UnixMicro()
	case storage.DeliveryDelivered:
		delivered, finished = at, at
	default:
		finished = at
	}
	_, err = tx.ExecContext(ctx, `UPDATE alert_deliveries SET state=?,attempt_count=?,next_attempt_at=?,claim_token=NULL,claimed_until=NULL,last_status=nullif(?,0),last_error=nullif(?,''),delivered_at=?,finished_at=? WHERE delivery_id=?`,
		next, count, nextAttempt, result.Status, result.Error, delivered, finished, deliveryID)
	if err != nil {
		return databaseError(err)
	}
	if err = tx.Commit(); err != nil {
		return databaseError(err)
	}
	return nil
}

func (s *Store) ReleaseAlertDelivery(ctx context.Context, deliveryID, token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.ExecContext(ctx, `UPDATE alert_deliveries SET state='pending',claim_token=NULL,claimed_until=NULL WHERE delivery_id=? AND state='claimed' AND claim_token=?`, deliveryID, token); err != nil {
		return databaseError(err)
	}
	return nil
}

func (s *Store) reader() *sql.DB {
	if s.readDB != nil {
		return s.readDB
	}
	return s.db
}

func (s *Store) AlertRecord(ctx context.Context, requestID string, attempt int) (storage.Record, error) {
	var r storage.Record
	var data string
	err := s.reader().QueryRowContext(ctx, `SELECT record_json FROM requests WHERE request_id=? AND attempt_index=?`, requestID, attempt).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return r, storage.ErrAlertSourceMissing
	}
	if err != nil {
		return r, storage.ErrUnavailable
	}
	if err = decodeRecord(data, &r); err != nil {
		return r, err
	}
	return r, nil
}

// AlertBudgets reads the checkpoint and the ledger written after it in one
// read transaction, so a concurrent checkpoint can never count a settlement
// twice or miss it.
func (s *Store) AlertBudgets(ctx context.Context, app, scope, subject string, anonymous bool, at time.Time) ([]storage.AlertBudget, error) {
	if !storage.RecordName.MatchString(app) || !storage.ValidSubject(scope, subject, anonymous) {
		return nil, storage.ErrInvalid
	}
	tx, err := s.reader().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, storage.ErrUnavailable
	}
	defer tx.Rollback()
	var covered int64
	if err = tx.QueryRowContext(ctx, `SELECT coalesce((SELECT covered_seq FROM limit_checkpoints WHERE id=1),0)`).Scan(&covered); err != nil {
		return nil, storage.ErrUnavailable
	}
	micros := at.UnixMicro()
	rows, err := tx.QueryContext(ctx, `SELECT dimension,window_start,window_end,spent FROM budget_state WHERE app_name=? AND scope=? AND subject=? AND anonymous=? AND window_start<=? AND window_end>?`, app, scope, subject, anonymous, micros, micros)
	if err != nil {
		return nil, storage.ErrUnavailable
	}
	var base []storage.AlertBudget
	for rows.Next() {
		b := storage.AlertBudget{Scope: scope, Subject: subject, Anonymous: anonymous}
		if err = rows.Scan(&b.Dimension, &b.WindowStart, &b.WindowEnd, &b.Spent); err != nil {
			rows.Close()
			return nil, storage.ErrUnavailable
		}
		base = append(base, b)
	}
	rows.Close()
	rows, err = tx.QueryContext(ctx, `SELECT payload_json FROM limit_ledger WHERE seq>? AND transition IN('settle','release') AND json_extract(payload_json,'$.app_name')=? ORDER BY seq`, covered, app)
	if err != nil {
		return nil, storage.ErrUnavailable
	}
	defer rows.Close()
	var ledger []storage.LedgerPayload
	for rows.Next() {
		var data string
		var p storage.LedgerPayload
		if err = rows.Scan(&data); err != nil {
			return nil, storage.ErrUnavailable
		}
		if err = json.Unmarshal([]byte(data), &p); err != nil {
			return nil, storage.ErrInvalid
		}
		ledger = append(ledger, p)
	}
	if err = rows.Err(); err != nil {
		return nil, storage.ErrUnavailable
	}
	return storage.FoldAlertBudgets(base, ledger, app, scope, subject, anonymous, micros), nil
}

func (s *Store) CountAlertEvents(ctx context.Context, app string, kind events.Type, from, until time.Time) (int64, error) {
	var n int64
	if err := s.reader().QueryRowContext(ctx, `SELECT count(*) FROM events WHERE app_name=? AND type=? AND occurred_at>? AND occurred_at<=?`, app, string(kind), from.UnixMicro(), until.UnixMicro()).Scan(&n); err != nil {
		return 0, storage.ErrUnavailable
	}
	return n, nil
}

func (s *Store) UsageBuckets(ctx context.Context, from, until time.Time) ([]storage.UsageBucket, error) {
	rows, err := s.reader().QueryContext(ctx, `SELECT bucket_start,app_name,cost_microusd FROM usage_5m WHERE bucket_start>=? AND bucket_start<? ORDER BY app_name,bucket_start`, from.UnixMicro(), until.UnixMicro())
	if err != nil {
		return nil, storage.ErrUnavailable
	}
	defer rows.Close()
	var out []storage.UsageBucket
	for rows.Next() {
		var b storage.UsageBucket
		var start int64
		if err = rows.Scan(&start, &b.App, &b.Cost); err != nil {
			return nil, storage.ErrUnavailable
		}
		b.Start = time.UnixMicro(start).UTC()
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *Store) AlertBacklog(ctx context.Context, now time.Time) (storage.AlertBacklog, error) {
	var b storage.AlertBacklog
	var oldest sql.NullInt64
	db := s.reader()
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM alert_inputs WHERE state<>'processed'`).Scan(&b.PendingInputs); err != nil {
		return b, storage.ErrUnavailable
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*),min(created_at) FROM alert_deliveries WHERE state IN('pending','claimed')`).Scan(&b.PendingDeliveries, &oldest); err != nil {
		return b, storage.ErrUnavailable
	}
	if oldest.Valid {
		b.OldestDelivery = max(0, now.Sub(time.UnixMicro(oldest.Int64)))
	}
	return b, nil
}

func (s *Store) RecentAlertOccurrences(ctx context.Context, limit int) ([]storage.AlertOccurrence, error) {
	if limit < 1 || limit > storage.MaxAlertBatch {
		return nil, storage.ErrInvalid
	}
	rows, err := s.reader().QueryContext(ctx, `SELECT `+occurrenceColumns+` FROM alert_occurrences o ORDER BY o.occurred_at DESC,o.occurrence_id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, storage.ErrUnavailable
	}
	defer rows.Close()
	var out []storage.AlertOccurrence
	for rows.Next() {
		o, err := scanOccurrence(rows)
		if err != nil {
			return nil, storage.ErrUnavailable
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// RetainAlerts removes finished work in small batches. Pending or claimed work
// is never removed, and an occurrence stays while it has deliveries or its
// window is still open, so it keeps deduplicating.
func (s *Store) RetainAlerts(ctx context.Context, now time.Time) error {
	inputs, terminal, usage := storage.AlertRetentionCutoffs(now)
	statements := []struct {
		query string
		args  []any
	}{
		{`DELETE FROM alert_inputs WHERE rowid IN(SELECT rowid FROM alert_inputs WHERE state='processed' AND processed_at<? LIMIT ?)`, []any{inputs.UnixMicro()}},
		{`DELETE FROM alert_deliveries WHERE rowid IN(SELECT rowid FROM alert_deliveries WHERE state IN('delivered','failed','canceled') AND finished_at<? LIMIT ?)`, []any{terminal.UnixMicro()}},
		{`DELETE FROM alert_occurrences WHERE rowid IN(SELECT o.rowid FROM alert_occurrences o WHERE o.created_at<? AND (o.window_end IS NULL OR o.window_end<?) AND NOT EXISTS(SELECT 1 FROM alert_deliveries d WHERE d.occurrence_id=o.occurrence_id) LIMIT ?)`, []any{terminal.UnixMicro(), now.UnixMicro()}},
		{`DELETE FROM usage_5m WHERE rowid IN(SELECT rowid FROM usage_5m WHERE bucket_start<? LIMIT ?)`, []any{usage.UnixMicro()}},
	}
	for _, st := range statements {
		for {
			s.mu.Lock()
			result, err := s.db.ExecContext(ctx, st.query, append(st.args, storage.AlertRetentionBatch)...)
			s.mu.Unlock()
			if err != nil {
				return databaseError(err)
			}
			n, err := result.RowsAffected()
			if err != nil {
				return databaseError(err)
			}
			if n < storage.AlertRetentionBatch {
				break
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
		}
	}
	return nil
}
