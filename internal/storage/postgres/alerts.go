package postgres

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
// 0004 migration, in the same transaction as the event or the request row.
// Every replica may evaluate and deliver: claims carry a lease and a token, and
// the unique keys, not the leases, are what deduplicate.

// deliveryClaimLock serializes delivery claims across replicas, which is what
// keeps a single delivery in flight per channel. Claims are short and never
// hold the lock while calling a receiver.
const deliveryClaimLock = 0x636f6d70616c7274

func (s *Store) ClaimAlertInputs(ctx context.Context, now time.Time, lease time.Duration, limit int) ([]storage.AlertInput, error) {
	if limit < 1 || limit > storage.MaxAlertBatch || lease <= 0 {
		return nil, storage.ErrInvalid
	}
	var out []storage.AlertInput
	err := s.inTx(ctx, func(tx *sql.Tx) (func(), error) {
		out = nil
		rows, err := tx.QueryContext(ctx, `SELECT event_id,event_type,enqueued_at FROM alert_inputs WHERE state='pending' OR (state='claimed' AND claimed_until<=$1) ORDER BY enqueued_at,event_id LIMIT $2 FOR UPDATE SKIP LOCKED`, micros(now), limit)
		if err != nil {
			return nil, databaseError(err)
		}
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
		until := micros(now.Add(lease))
		for i := range out {
			if _, err = tx.ExecContext(ctx, `UPDATE alert_inputs SET state='claimed',claim_token=$1,claimed_until=$2 WHERE event_id=$3`, out[i].ClaimToken, until, out[i].EventID); err != nil {
				return nil, databaseError(err)
			}
			if out[i].Event, err = loadEvent(ctx, tx, out[i].EventID); err != nil {
				return nil, err
			}
		}
		return nil, nil
	})
	return out, err
}

func loadEvent(ctx context.Context, tx *sql.Tx, id string) (*events.Event, error) {
	var e events.Event
	var at int64
	var payload, hash string
	var anonymous sql.NullBool
	err := tx.QueryRowContext(ctx, `SELECT event_id,type,occurred_at,coalesce(request_id,''),coalesce(app_name,''),coalesce(user_hash,''),anonymous,payload_json::text FROM events WHERE event_id=$1`, id).
		Scan(&e.ID, &e.Type, &at, &e.RequestID, &e.App, &hash, &anonymous, &payload)
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
	e.At = time.UnixMicro(at).UTC()
	if err = json.Unmarshal([]byte(payload), &e.Payload); err != nil || e.Validate() != nil {
		return nil, storage.ErrInvalid
	}
	return &e, nil
}

func (s *Store) CompleteAlertInput(ctx context.Context, eventID, token string, occurrences []storage.AlertOccurrence, channels []storage.AlertChannelRef, now time.Time) error {
	if err := validateAlertWrite(occurrences, channels); err != nil {
		return err
	}
	return s.inTx(ctx, func(tx *sql.Tx) (func(), error) {
		var state string
		var current sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT state,claim_token FROM alert_inputs WHERE event_id=$1 FOR UPDATE`, eventID).Scan(&state, &current)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && (state != "claimed" || current.String != token)) {
			return nil, storage.ErrClaimLost
		}
		if err != nil {
			return nil, databaseError(err)
		}
		if err = insertOccurrences(ctx, tx, occurrences, channels, now); err != nil {
			return nil, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE alert_inputs SET state='processed',processed_at=$1,claim_token=NULL,claimed_until=NULL WHERE event_id=$2`, micros(now), eventID); err != nil {
			return nil, databaseError(err)
		}
		return nil, nil
	})
}

func (s *Store) ReleaseAlertInput(ctx context.Context, eventID, token string) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE alert_inputs SET state='pending',claim_token=NULL,claimed_until=NULL WHERE event_id=$1 AND state='claimed' AND claim_token=$2`, eventID, token); err != nil {
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
	return s.inTx(ctx, func(tx *sql.Tx) (func(), error) {
		return nil, insertOccurrences(ctx, tx, occurrences, channels, now)
	})
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
// deliveries. Two replicas racing on one key meet at the unique constraint:
// the loser sees zero rows and adds nothing.
func insertOccurrences(ctx context.Context, tx *sql.Tx, occurrences []storage.AlertOccurrence, channels []storage.AlertChannelRef, now time.Time) error {
	if len(occurrences) == 0 {
		return nil
	}
	if len(channels) > 0 {
		var open int64
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM alert_deliveries WHERE state IN ('pending','claimed')`).Scan(&open); err != nil {
			return databaseError(err)
		}
		if open+int64(len(occurrences)*len(channels)) > storage.MaxAlertDeliveries {
			return storage.ErrAlertBacklogFull
		}
	}
	at := micros(now)
	for _, o := range occurrences {
		var anonymous any
		if o.Anonymous != nil {
			anonymous = *o.Anonymous
		}
		var threshold any
		if o.ThresholdPercent > 0 {
			threshold = o.ThresholdPercent
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO alert_occurrences (occurrence_id,dedupe_key,trigger,occurred_at,app_name,scope,user_hash,anonymous,dimension,window_start,window_end,threshold_percent,payload_json,created_at)
VALUES ($1,$2,$3,$4,nullif($5,''),$6,nullif($7,''),$8,nullif($9,''),$10,$11,$12,$13,$14) ON CONFLICT DO NOTHING`,
			o.ID, o.DedupeKey, o.Trigger, micros(o.OccurredAt), o.App, o.Scope, o.UserHash, anonymous, o.Dimension, optionalMicros(o.WindowStart), optionalMicros(o.WindowEnd), threshold, string(o.Payload), at)
		if err != nil {
			return databaseError(err)
		}
		if n, err := result.RowsAffected(); err != nil || n == 0 {
			continue
		}
		for _, c := range channels {
			if _, err = tx.ExecContext(ctx, `INSERT INTO alert_deliveries (delivery_id,occurrence_id,channel_name,channel_type,state,attempt_count,next_attempt_at,created_at) VALUES ($1,$2,$3,$4,'pending',0,$5,$5) ON CONFLICT DO NOTHING`,
				storage.DeliveryID(o.ID, c.Name), o.ID, c.Name, c.Type, at); err != nil {
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
	return micros(*t)
}

const occurrenceColumns = `o.occurrence_id,o.dedupe_key,o.trigger,o.occurred_at,coalesce(o.app_name,''),o.scope,coalesce(o.user_hash,''),o.anonymous,coalesce(o.dimension,''),o.window_start,o.window_end,coalesce(o.threshold_percent,0),o.payload_json,o.created_at`

type rowScanner interface{ Scan(...any) error }

func scanOccurrence(row rowScanner, prefix ...any) (storage.AlertOccurrence, error) {
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

// ClaimAlertDeliveries takes, for each channel without a delivery in flight
// anywhere in the cluster, its oldest due delivery.
func (s *Store) ClaimAlertDeliveries(ctx context.Context, now time.Time, lease time.Duration, limit int) ([]storage.AlertDelivery, error) {
	if limit < 1 || limit > storage.MaxAlertBatch || lease <= 0 {
		return nil, storage.ErrInvalid
	}
	var out []storage.AlertDelivery
	err := s.inTx(ctx, func(tx *sql.Tx) (func(), error) {
		out = nil
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, deliveryClaimLock); err != nil {
			return nil, databaseError(err)
		}
		at := micros(now)
		busy := map[string]bool{}
		rows, err := tx.QueryContext(ctx, `SELECT DISTINCT channel_name FROM alert_deliveries WHERE state='claimed' AND claimed_until>$1`, at)
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
WHERE (d.state='pending' OR (d.state='claimed' AND d.claimed_until<=$1)) AND d.next_attempt_at<=$1
ORDER BY d.created_at,d.delivery_id LIMIT 1000`, at)
		if err != nil {
			return nil, databaseError(err)
		}
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
		until := micros(now.Add(lease))
		for i := range out {
			if _, err = tx.ExecContext(ctx, `UPDATE alert_deliveries SET state='claimed',claim_token=$1,claimed_until=$2 WHERE delivery_id=$3`, out[i].ClaimToken, until, out[i].ID); err != nil {
				return nil, databaseError(err)
			}
			out[i].State = storage.DeliveryClaimed
		}
		return nil, nil
	})
	return out, err
}

func (s *Store) FinishAlertDelivery(ctx context.Context, deliveryID, token string, result storage.AlertResult, now time.Time) error {
	if err := storage.ValidateAlertResult(result); err != nil {
		return err
	}
	return s.inTx(ctx, func(tx *sql.Tx) (func(), error) {
		var state string
		var current sql.NullString
		var attempts int
		err := tx.QueryRowContext(ctx, `SELECT state,claim_token,attempt_count FROM alert_deliveries WHERE delivery_id=$1 FOR UPDATE`, deliveryID).Scan(&state, &current, &attempts)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && (state != storage.DeliveryClaimed || current.String != token)) {
			return nil, storage.ErrClaimLost
		}
		if err != nil {
			return nil, databaseError(err)
		}
		next, count := storage.NextDeliveryState(attempts, result)
		at := micros(now)
		nextAttempt := at
		var delivered, finished, status, category any
		switch next {
		case storage.DeliveryPending:
			nextAttempt = micros(result.RetryAt)
		case storage.DeliveryDelivered:
			delivered, finished = at, at
		default:
			finished = at
		}
		if result.Status != 0 {
			status = result.Status
		}
		if result.Error != "" {
			category = result.Error
		}
		_, err = tx.ExecContext(ctx, `UPDATE alert_deliveries SET state=$1,attempt_count=$2,next_attempt_at=$3,claim_token=NULL,claimed_until=NULL,last_status=$4,last_error=$5,delivered_at=$6,finished_at=$7 WHERE delivery_id=$8`,
			next, count, nextAttempt, status, category, delivered, finished, deliveryID)
		if err != nil {
			return nil, databaseError(err)
		}
		return nil, nil
	})
}

func (s *Store) ReleaseAlertDelivery(ctx context.Context, deliveryID, token string) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE alert_deliveries SET state='pending',claim_token=NULL,claimed_until=NULL WHERE delivery_id=$1 AND state='claimed' AND claim_token=$2`, deliveryID, token); err != nil {
		return databaseError(err)
	}
	return nil
}

func (s *Store) AlertRecord(ctx context.Context, requestID string, attempt int) (storage.Record, error) {
	var r storage.Record
	var data string
	err := s.db.QueryRowContext(ctx, `SELECT record_json::text FROM requests WHERE request_id=$1 AND attempt_index=$2`, requestID, attempt).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return r, storage.ErrAlertSourceMissing
	}
	if err != nil {
		return r, storage.ErrUnavailable
	}
	_, r, err = canonicalRecord(data)
	return r, err
}

// AlertBudgets reads the checkpoint and the ledger written after it in one
// repeatable read transaction, so a concurrent checkpoint is seen whole or
// not at all.
func (s *Store) AlertBudgets(ctx context.Context, app, scope, subject string, anonymous bool, at time.Time) ([]storage.AlertBudget, error) {
	if !storage.RecordName.MatchString(app) || !storage.ValidSubject(scope, subject, anonymous) {
		return nil, storage.ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, storage.ErrUnavailable
	}
	defer tx.Rollback()
	var covered int64
	if err = tx.QueryRowContext(ctx, `SELECT coalesce((SELECT covered_seq FROM limit_checkpoints WHERE id=1),0)`).Scan(&covered); err != nil {
		return nil, storage.ErrUnavailable
	}
	when := micros(at)
	rows, err := tx.QueryContext(ctx, `SELECT dimension,window_start,window_end,spent FROM budget_state WHERE app_name=$1 AND scope=$2 AND subject=$3 AND anonymous=$4 AND window_start<=$5 AND window_end>$5`, app, scope, subject, anonymous, when)
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
	rows, err = tx.QueryContext(ctx, `SELECT payload_json::text FROM limit_ledger WHERE seq>$1 AND transition IN ('settle','release') AND payload_json->>'app_name'=$2 ORDER BY seq`, covered, app)
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
	return storage.FoldAlertBudgets(base, ledger, app, scope, subject, anonymous, when), nil
}

func (s *Store) CountAlertEvents(ctx context.Context, app string, kind events.Type, from, until time.Time) (int64, error) {
	var n int64
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM events WHERE app_name=$1 AND type=$2 AND occurred_at>$3 AND occurred_at<=$4`, app, string(kind), micros(from), micros(until)).Scan(&n); err != nil {
		return 0, storage.ErrUnavailable
	}
	return n, nil
}

func (s *Store) UsageBuckets(ctx context.Context, from, until time.Time) ([]storage.UsageBucket, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT bucket_start,app_name,cost_microusd FROM usage_5m WHERE bucket_start>=$1 AND bucket_start<$2 ORDER BY app_name,bucket_start`, micros(from), micros(until))
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
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM alert_inputs WHERE state<>'processed'`).Scan(&b.PendingInputs); err != nil {
		return b, storage.ErrUnavailable
	}
	if err := s.db.QueryRowContext(ctx, `SELECT count(*),min(created_at) FROM alert_deliveries WHERE state IN ('pending','claimed')`).Scan(&b.PendingDeliveries, &oldest); err != nil {
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
	rows, err := s.db.QueryContext(ctx, `SELECT `+occurrenceColumns+` FROM alert_occurrences o ORDER BY o.occurred_at DESC,o.occurrence_id DESC LIMIT $1`, limit)
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
// window is still open.
func (s *Store) RetainAlerts(ctx context.Context, now time.Time) error {
	inputs, terminal, usage := storage.AlertRetentionCutoffs(now)
	statements := []struct {
		query string
		args  []any
	}{
		{`DELETE FROM alert_inputs WHERE event_id IN (SELECT event_id FROM alert_inputs WHERE state='processed' AND processed_at<$1 LIMIT $2)`, []any{micros(inputs)}},
		{`DELETE FROM alert_deliveries WHERE delivery_id IN (SELECT delivery_id FROM alert_deliveries WHERE state IN ('delivered','failed','canceled') AND finished_at<$1 LIMIT $2)`, []any{micros(terminal)}},
		{`DELETE FROM alert_occurrences WHERE occurrence_id IN (SELECT o.occurrence_id FROM alert_occurrences o WHERE o.created_at<$1 AND (o.window_end IS NULL OR o.window_end<$2) AND NOT EXISTS (SELECT 1 FROM alert_deliveries d WHERE d.occurrence_id=o.occurrence_id) LIMIT $3)`, []any{micros(terminal), micros(now)}},
		{`DELETE FROM usage_5m WHERE (bucket_start,app_name) IN (SELECT bucket_start,app_name FROM usage_5m WHERE bucket_start<$1 LIMIT $2)`, []any{micros(usage)}},
	}
	for _, st := range statements {
		for {
			result, err := s.db.ExecContext(ctx, st.query, append(st.args, storage.AlertRetentionBatch)...)
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
