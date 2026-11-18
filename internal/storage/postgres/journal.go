package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/events"
	"github.com/JoaoVictorVM/comporta/internal/storage"
)

// jsonb reorders keys, so a stored document is compared through its decoded
// value, re-encoded by the same marshaller that wrote it.
func canonicalRecord(text string) (string, storage.Record, error) {
	var r storage.Record
	if err := storage.DecodeRecord(text, &r); err != nil {
		return "", r, err
	}
	data, _ := json.Marshal(r)
	return string(data), r, nil
}

func canonicalQuote(text string) (string, error) {
	var q storage.Quote
	if err := storage.DecodeStrict(text, &q); err != nil {
		return "", err
	}
	data, _ := json.Marshal(q)
	return string(data), nil
}

func (s *Store) Admit(ctx context.Context, r storage.Record) error {
	if err := storage.ValidateRecord(r); err != nil {
		return err
	}
	return s.inTx(ctx, func(tx *sql.Tx) (func(), error) {
		_, err := s.admitJournal(ctx, tx, r)
		return nil, err
	})
}

func (s *Store) admitJournal(ctx context.Context, tx *sql.Tx, r storage.Record) (bool, error) {
	data, _ := json.Marshal(r)
	quote, _ := json.Marshal(r.Quote)
	owner := s.currentOwner()
	// The bound on unfinished entries is checked in the same statement, and the
	// count stops at the limit, so it never scans a large journal.
	var inserted bool
	err := tx.QueryRowContext(ctx, `WITH capacity AS (SELECT count(*) AS n FROM (SELECT 1 FROM metering_journal WHERE state='admitted' LIMIT $8) x),
ins AS (INSERT INTO metering_journal(request_id,attempt_index,admitted_at,state,metadata_json,quote_json,result_json,owner_id,owner_fence)
SELECT $1,$2,$3,'admitted',$4,$5,NULL,$6,$7 FROM capacity WHERE n<$8 ON CONFLICT(request_id,attempt_index) DO NOTHING RETURNING 1)
SELECT EXISTS(SELECT 1 FROM ins)`, r.RequestID, r.AttemptIndex, micros(r.StartedAt), string(data), string(quote), owner.ID, owner.Fence, s.journalLimit).Scan(&inserted)
	if err != nil {
		return false, databaseError(err)
	}
	if inserted {
		return true, nil
	}
	var old string
	err = tx.QueryRowContext(ctx, `SELECT metadata_json::text FROM metering_journal WHERE request_id=$1 AND attempt_index=$2`, r.RequestID, r.AttemptIndex).Scan(&old)
	if errors.Is(err, sql.ErrNoRows) {
		return false, storage.ErrUnavailable
	}
	if err != nil {
		return false, databaseError(err)
	}
	canonical, _, err := canonicalRecord(old)
	if err != nil {
		return false, err
	}
	if canonical == string(data) {
		return false, nil
	}
	return false, storage.ErrConflict
}

func (s *Store) Complete(ctx context.Context, r storage.Record) error {
	if err := storage.ValidateRecord(r); err != nil {
		return err
	}
	return s.inTx(ctx, func(tx *sql.Tx) (func(), error) { return nil, completeOne(ctx, tx, r) })
}

func completeOne(ctx context.Context, tx *sql.Tx, r storage.Record) error {
	data, _ := json.Marshal(r)
	var prior sql.NullString
	var metadata string
	if err := tx.QueryRowContext(ctx, `SELECT metadata_json::text,result_json::text FROM metering_journal WHERE request_id=$1 AND attempt_index=$2 FOR UPDATE`, r.RequestID, r.AttemptIndex).Scan(&metadata, &prior); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return storage.ErrConflict
		}
		return databaseError(err)
	}
	_, admission, err := canonicalRecord(metadata)
	if err != nil {
		return err
	}
	if !storage.SameAdmission(admission, r) {
		return storage.ErrConflict
	}
	if prior.Valid {
		canonical, _, err := canonicalRecord(prior.String)
		if err != nil {
			return err
		}
		if canonical == string(data) {
			return nil
		}
		return storage.ErrConflict
	}
	if _, err := tx.ExecContext(ctx, `UPDATE metering_journal SET state='completed',result_json=$1 WHERE request_id=$2 AND attempt_index=$3`, string(data), r.RequestID, r.AttemptIndex); err != nil {
		return databaseError(err)
	}
	return nil
}

// Pending returns the journal entries of this process. Entries of other
// processes are theirs to materialize, or the recovery of a dead owner's.
func (s *Store) Pending(ctx context.Context) ([]storage.Pending, error) {
	return s.PendingOf(ctx, s.currentOwner().ID)
}

// PendingOf returns the journal entries left by one owner.
func (s *Store) PendingOf(ctx context.Context, owner string) ([]storage.Pending, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT request_id,attempt_index,metadata_json::text,quote_json::text,result_json::text FROM metering_journal WHERE owner_id=$1 ORDER BY admitted_at,request_id,attempt_index`, owner)
	if err != nil {
		return nil, storage.ErrUnavailable
	}
	defer rows.Close()
	var out []storage.Pending
	for rows.Next() {
		var requestID, data, quote string
		var index int
		var result sql.NullString
		var p storage.Pending
		if err = rows.Scan(&requestID, &index, &data, &quote, &result); err != nil {
			return nil, storage.ErrUnavailable
		}
		if _, p.Admission, err = canonicalRecord(data); err != nil {
			return nil, storage.ErrInvalid
		}
		expected, _ := json.Marshal(p.Admission.Quote)
		canonical, err := canonicalQuote(quote)
		if err != nil || p.Admission.RequestID != requestID || p.Admission.AttemptIndex != index || canonical != string(expected) {
			return nil, storage.ErrInvalid
		}
		if result.Valid {
			_, record, err := canonicalRecord(result.String)
			if err != nil {
				return nil, storage.ErrInvalid
			}
			if record.RequestID != requestID || record.AttemptIndex != index || !storage.SameAdmission(p.Admission, record) {
				return nil, storage.ErrConflict
			}
			p.Result = &record
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Commit materializes records and their aggregates in one transaction. A
// record already materialized, by this process or by the recovery of another,
// is confirmed and never counted twice.
func (s *Store) Commit(ctx context.Context, records []storage.Record) error {
	return s.inTx(ctx, func(tx *sql.Tx) (func(), error) {
		var notifications []events.Event
		for _, r := range records {
			if err := storage.ValidateRecord(r); err != nil {
				return nil, err
			}
			if r.Outcome == "loop_cut" {
				return nil, storage.ErrInvalid
			}
			inserted := false
			if err := commitRecord(ctx, tx, r, &inserted); err != nil {
				return nil, err
			}
			if inserted {
				notifications = append(notifications, storage.EventFor(r))
				if r.Outcome == "cached" && r.AttemptIndex > 0 {
					notifications = append(notifications, storage.CachedEvent(r))
				}
			}
		}
		return func() {
			for _, e := range notifications {
				s.Bus.Notify(e)
			}
		}, nil
	})
}

func commitRecord(ctx context.Context, tx *sql.Tx, r storage.Record, inserted *bool) error {
	var op operationRow
	if r.AttemptIndex > 0 {
		var err error
		if op, err = readOperation(ctx, tx, r.RequestID); err != nil {
			return err
		}
		count, err := op.requestCount(r.AttemptIndex, r.RequestCount)
		if err != nil {
			return err
		}
		if r.Recovered && count != r.RequestCount {
			r.RequestCount = count
			if count == 0 {
				r.TerminalErrorCode, r.TerminalStatus = "", nil
			}
		}
		if r.RequestCount != count {
			return storage.ErrConflict
		}
	}
	data, _ := json.Marshal(r)
	quote, _ := json.Marshal(r.Quote)
	var old string
	err := tx.QueryRowContext(ctx, `SELECT record_json::text FROM requests WHERE request_id=$1 AND attempt_index=$2`, r.RequestID, r.AttemptIndex).Scan(&old)
	if err == nil {
		canonical, _, err := canonicalRecord(old)
		if err != nil {
			return err
		}
		if canonical == string(data) {
			return nil
		}
		return storage.ErrConflict
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return databaseError(err)
	}
	if r.AttemptIndex == 0 {
		return commitBlocked(ctx, tx, r, string(data), string(quote), inserted)
	}
	var metadata string
	var exact sql.NullString
	if err = tx.QueryRowContext(ctx, `SELECT metadata_json::text,result_json::text FROM metering_journal WHERE request_id=$1 AND attempt_index=$2 FOR UPDATE`, r.RequestID, r.AttemptIndex).Scan(&metadata, &exact); err != nil {
		return storage.ErrConflict
	}
	_, admission, err := canonicalRecord(metadata)
	if err != nil {
		return err
	}
	if !storage.SameAdmission(admission, r) {
		return storage.ErrConflict
	}
	if exact.Valid {
		_, expected, err := canonicalRecord(exact.String)
		if err != nil {
			return err
		}
		expected.Recovered = r.Recovered
		expected.RequestCount, expected.TerminalErrorCode, expected.TerminalStatus = r.RequestCount, r.TerminalErrorCode, r.TerminalStatus
		canonical, _ := json.Marshal(expected)
		if string(canonical) != string(data) {
			return storage.ErrConflict
		}
	}
	if err = insertRequest(ctx, tx, r, string(data), string(quote)); err != nil {
		return err
	}
	if op.exists && r.AttemptIndex == op.last {
		if err = closeOperation(ctx, tx, r); err != nil {
			return err
		}
	}
	for _, scope := range []string{"app", "user"} {
		subject, anonymous := "", false
		if scope == "user" {
			subject, anonymous = r.UserHash, r.Anonymous
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO usage_daily VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
ON CONFLICT(day,timezone,app_name,scope,subject,anonymous) DO UPDATE SET requests=usage_daily.requests+excluded.requests,attempts=usage_daily.attempts+excluded.attempts,
input_tokens=usage_daily.input_tokens+excluded.input_tokens,output_tokens=usage_daily.output_tokens+excluded.output_tokens,
cache_write_tokens=usage_daily.cache_write_tokens+excluded.cache_write_tokens,cache_read_tokens=usage_daily.cache_read_tokens+excluded.cache_read_tokens,
usage_missing_count=usage_daily.usage_missing_count+excluded.usage_missing_count,recovered_count=usage_daily.recovered_count+excluded.recovered_count,
cost_microusd=usage_daily.cost_microusd+excluded.cost_microusd`,
			r.Day, r.Timezone, r.AppName, scope, subject, anonymous, r.RequestCount, 1, storage.Value(r.Input), storage.Value(r.Output), storage.Value(r.CacheWrite), storage.Value(r.CacheRead), boolInt(r.UsageMissing), boolInt(r.Recovered), int64(r.Cost))
		if err != nil {
			return databaseError(err)
		}
	}
	event := storage.CompletionEvent(r)
	payload, _ := json.Marshal(event.Payload)
	if _, err = tx.ExecContext(ctx, `INSERT INTO events(event_id,type,occurred_at,request_id,attempt_index,app_name,payload_json) VALUES($1,'request_completed',$2,$3,$4,$5,$6)`,
		event.ID, micros(r.FinishedAt), r.RequestID, r.AttemptIndex, r.AppName, string(payload)); err != nil {
		return databaseError(err)
	}
	if r.Outcome == "cached" {
		hit := storage.CachedEvent(r)
		payload, _ := json.Marshal(hit.Payload)
		if _, err = tx.ExecContext(ctx, `INSERT INTO events(event_id,type,occurred_at,request_id,attempt_index,app_name,payload_json) VALUES($1,'cached',$2,$3,$4,$5,$6)`,
			hit.ID, micros(r.FinishedAt), r.RequestID, r.AttemptIndex, r.AppName, string(payload)); err != nil {
			return databaseError(err)
		}
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM metering_journal WHERE request_id=$1 AND attempt_index=$2`, r.RequestID, r.AttemptIndex); err != nil {
		return databaseError(err)
	}
	*inserted = true
	return nil
}

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

const requestColumns = `request_id,attempt_index,started_at,finished_at,app_name,user_hash,anonymous,operation,provider,model_requested,model_used,upstream_status,downstream_status,outcome,error_class,error_code,input_tokens,output_tokens,cache_write_tokens,cache_read_tokens,raw_input_total,counted_input_tokens,cache_write_5m_tokens,cache_write_1h_tokens,cost_microusd,reserved_cost_microusd,usage_missing,recovered,estimate_exceeded,request_count,latency_us,upstream_ttfb_us,client_ttfb_us,usage_day,timezone,price_quote_json,record_json`

func placeholders(n int) string { return placeholdersFrom(1, n) }

func placeholdersFrom(first, n int) string {
	var b strings.Builder
	for i := first; i < first+n; i++ {
		if i > first {
			b.WriteByte(',')
		}
		b.WriteByte('$')
		b.WriteString(strconv.Itoa(i))
	}
	return b.String()
}

func insertRequest(ctx context.Context, tx *sql.Tx, r storage.Record, data, quote string) error {
	args := []any{r.RequestID, r.AttemptIndex, micros(r.StartedAt), micros(r.FinishedAt), r.AppName, r.UserHash, r.Anonymous, r.Operation, r.Provider, r.ModelRequested, r.ModelUsed,
		r.UpstreamStatus, r.DownstreamStatus, r.Outcome, r.ErrorClass, r.ErrorCode, r.Input, r.Output, r.CacheWrite, r.CacheRead, r.RawInputTotal, r.CountedInput, r.CacheWrite5m, r.CacheWrite1h,
		int64(r.Cost), int64(r.ReservedCost), r.UsageMissing, r.Recovered, r.EstimateExceeded, r.RequestCount, r.LatencyUS, r.UpstreamTTFB, r.ClientTTFB, r.Day, r.Timezone, quote, data}
	if _, err := tx.ExecContext(ctx, `INSERT INTO requests(`+requestColumns+`) VALUES(`+placeholders(len(args))+`)`, args...); err != nil {
		return databaseError(err)
	}
	return nil
}

func commitBlocked(ctx context.Context, tx *sql.Tx, r storage.Record, data, quote string, inserted *bool) error {
	if err := insertRequest(ctx, tx, r, data, quote); err != nil {
		return err
	}
	event := storage.BlockedEvent(r)
	payload, _ := json.Marshal(event.Payload)
	if _, err := tx.ExecContext(ctx, `INSERT INTO events(event_id,type,occurred_at,request_id,attempt_index,app_name,payload_json) VALUES($1,'blocked',$2,$3,0,$4,$5)`,
		event.ID, micros(r.FinishedAt), r.RequestID, r.AppName, string(payload)); err != nil {
		return databaseError(err)
	}
	*inserted = true
	return nil
}

// RecoverOwner materializes what a dead owner left in the journal: an exact
// result where one was recorded, otherwise the reserved estimate as a
// recovered attempt with unknown usage. Nothing is ever sent again.
func (s *Store) RecoverOwner(ctx context.Context, owner string, now time.Time) error {
	pending, err := s.PendingOf(ctx, owner)
	if err != nil {
		return err
	}
	batch := make([]storage.Record, 0, 1000)
	for _, p := range pending {
		batch = append(batch, RecoveredRecord(p, now))
		if len(batch) == 1000 {
			if err = s.Commit(ctx, batch); err != nil {
				return err
			}
			batch = batch[:0]
		}
	}
	if len(batch) > 0 {
		return s.Commit(ctx, batch)
	}
	return nil
}

// RecoveredRecord is the record a recovery materializes for one journal entry.
func RecoveredRecord(p storage.Pending, now time.Time) storage.Record {
	r := p.Admission
	if p.Result != nil {
		r = *p.Result
		r.Recovered = true
		return r
	}
	r.FinishedAt = now.UTC()
	if r.FinishedAt.Before(r.StartedAt) {
		r.FinishedAt = r.StartedAt
	}
	r.Cost, r.UsageMissing, r.Recovered, r.RequestCount = r.ReservedCost, true, true, 1
	if r.Outcome != "downgraded" && r.Outcome != "fallback" {
		r.Outcome = "ok"
	}
	return r
}
