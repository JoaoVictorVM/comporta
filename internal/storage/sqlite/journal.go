package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/JoaoVictorVM/comporta/internal/events"
	"github.com/JoaoVictorVM/comporta/internal/storage"
	"strings"
	"time"
)

var hexID = storage.HexID

// The shared rules live in package storage; these names keep the SQLite code
// unchanged.
var (
	validate        = storage.ValidateRecord
	decodeRecord    = storage.DecodeRecord
	sameAdmission   = storage.SameAdmission
	blockedEvent    = storage.BlockedEvent
	eventFor        = storage.EventFor
	completionEvent = storage.CompletionEvent
	cachedEvent     = storage.CachedEvent
	value           = storage.Value
	recordName      = storage.RecordName
	category        = storage.Category
)

func (s *Store) Admit(ctx context.Context, r storage.Record) error {
	if err := validate(r); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	data, _ := json.Marshal(r)
	quote, _ := json.Marshal(r.Quote)
	var old string
	err := s.db.QueryRowContext(ctx, `SELECT metadata_json FROM metering_journal WHERE request_id=? AND attempt_index=?`, r.RequestID, r.AttemptIndex).Scan(&old)
	if err == nil {
		if old == string(data) {
			return nil
		}
		return storage.ErrConflict
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return databaseError(err)
	}
	if s.journalCount >= 50000 {
		return databaseError(err)
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO metering_journal VALUES(?,?,?,'admitted',?,?,NULL)`, r.RequestID, r.AttemptIndex, r.StartedAt.UnixMicro(), string(data), string(quote))
	if err != nil {
		return databaseError(err)
	}
	s.journalCount++
	return nil
}

func (s *Store) Complete(ctx context.Context, r storage.Record) error {
	if err := validate(r); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return databaseError(err)
	}
	defer tx.Rollback()
	if err = completeOne(ctx, tx, r); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return databaseError(err)
	}
	return nil
}

func completeOne(ctx context.Context, tx *sql.Tx, r storage.Record) error {
	data, _ := json.Marshal(r)
	var prior sql.NullString
	var metadata string
	if err := tx.QueryRowContext(ctx, `SELECT metadata_json,result_json FROM metering_journal WHERE request_id=? AND attempt_index=?`, r.RequestID, r.AttemptIndex).Scan(&metadata, &prior); err != nil {
		return databaseError(err)
	}
	var admission storage.Record
	if err := decodeRecord(metadata, &admission); err != nil {
		return err
	}
	if !sameAdmission(admission, r) {
		return storage.ErrConflict
	}
	if prior.Valid {
		if prior.String == string(data) {
			return nil
		}
		return storage.ErrConflict
	}
	_, err := tx.ExecContext(ctx, `UPDATE metering_journal SET state='completed',result_json=? WHERE request_id=? AND attempt_index=?`, string(data), r.RequestID, r.AttemptIndex)
	if err != nil {
		return databaseError(err)
	}
	return nil
}

func (s *Store) Pending(ctx context.Context) ([]storage.Pending, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT request_id,attempt_index,metadata_json,quote_json,result_json FROM metering_journal ORDER BY admitted_at,request_id,attempt_index`)
	if err != nil {
		return nil, storage.ErrUnavailable
	}
	defer rows.Close()
	var out []storage.Pending
	for rows.Next() {
		var data string
		var requestID, quote string
		var index int
		var result sql.NullString
		var p storage.Pending
		if err = rows.Scan(&requestID, &index, &data, &quote, &result); err != nil {
			return nil, storage.ErrUnavailable
		}
		if err = decodeRecord(data, &p.Admission); err != nil {
			return nil, storage.ErrInvalid
		}
		if err = validate(p.Admission); err != nil {
			return nil, err
		}
		expectedQuote, _ := json.Marshal(p.Admission.Quote)
		if p.Admission.RequestID != requestID || p.Admission.AttemptIndex != index || quote != string(expectedQuote) {
			return nil, storage.ErrInvalid
		}
		if result.Valid {
			p.Result = new(storage.Record)
			if err = decodeRecord(result.String, p.Result); err != nil {
				return nil, storage.ErrInvalid
			}
			if err = validate(*p.Result); err != nil {
				return nil, err
			}
			if p.Result.RequestID != requestID || p.Result.AttemptIndex != index || !sameAdmission(p.Admission, *p.Result) {
				return nil, storage.ErrConflict
			}
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) Commit(ctx context.Context, records []storage.Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return databaseError(err)
	}
	defer tx.Rollback()
	// Only a database of an earlier release, read in tests before its
	// migration, lacks the sessions table.
	sessions, err := hasTable(ctx, tx, "request_operations")
	if err != nil {
		return err
	}
	var notifications []events.Event
	for _, r := range records {
		if err = validate(r); err != nil {
			return err
		}
		// Loop cuts go through CommitLocalCut with their own event; here they
		// would gain a blocked event as well.
		if r.Outcome == "loop_cut" {
			return storage.ErrInvalid
		}
		inserted := false
		if err = commitRecord(ctx, tx, r, sessions, &inserted); err != nil {
			return err
		}
		if inserted {
			notifications = append(notifications, eventFor(r))
			if r.Outcome == "cached" && r.AttemptIndex > 0 {
				notifications = append(notifications, cachedEvent(r))
			}
		}
	}
	var pendingCount int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM metering_journal`).Scan(&pendingCount); err != nil {
		return databaseError(err)
	}
	if err = tx.Commit(); err != nil {
		return databaseError(err)
	}
	s.journalCount = pendingCount
	for _, e := range notifications {
		s.Bus.Notify(e)
	}
	return nil
}

func commitRecord(ctx context.Context, tx *sql.Tx, r storage.Record, sessions bool, inserted *bool) error {
	// The session of a request that fell back decides which attempt is terminal.
	// A recovered record takes that role; a live one must already carry it.
	var op operationRow
	if r.AttemptIndex > 0 && sessions {
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
	err := tx.QueryRowContext(ctx, `SELECT record_json FROM requests WHERE request_id=? AND attempt_index=?`, r.RequestID, r.AttemptIndex).Scan(&old)
	if err == nil {
		if old == string(data) {
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
	if err = tx.QueryRowContext(ctx, `SELECT metadata_json,result_json FROM metering_journal WHERE request_id=? AND attempt_index=?`, r.RequestID, r.AttemptIndex).Scan(&metadata, &exact); err != nil {
		return storage.ErrConflict
	}
	var admission storage.Record
	if err = decodeRecord(metadata, &admission); err != nil {
		return err
	}
	if !sameAdmission(admission, r) {
		return storage.ErrConflict
	}
	if exact.Valid {
		var expected storage.Record
		if err = decodeRecord(exact.String, &expected); err != nil {
			return err
		}
		// The journal holds the attempt's own result. Whether it ended the logical
		// request is decided afterwards, so the role fields come from the record.
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
		subject := ""
		anonymous := false
		if scope == "user" {
			subject = r.UserHash
			anonymous = r.Anonymous
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO usage_daily VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(day,timezone,app_name,scope,subject,anonymous) DO UPDATE SET requests=requests+excluded.requests,attempts=attempts+excluded.attempts,input_tokens=input_tokens+excluded.input_tokens,output_tokens=output_tokens+excluded.output_tokens,cache_write_tokens=cache_write_tokens+excluded.cache_write_tokens,cache_read_tokens=cache_read_tokens+excluded.cache_read_tokens,usage_missing_count=usage_missing_count+excluded.usage_missing_count,recovered_count=recovered_count+excluded.recovered_count,cost_microusd=cost_microusd+excluded.cost_microusd`, r.Day, r.Timezone, r.AppName, scope, subject, anonymous, r.RequestCount, 1, value(r.Input), value(r.Output), value(r.CacheWrite), value(r.CacheRead), r.UsageMissing, r.Recovered, r.Cost)
		if err != nil {
			return databaseError(err)
		}
	}
	event := completionEvent(r)
	payload, _ := json.Marshal(event.Payload)
	_, err = tx.ExecContext(ctx, `INSERT INTO events(event_id,type,occurred_at,request_id,attempt_index,app_name,payload_json) VALUES(?,'request_completed',?,?,?,?,?)`, event.ID, r.FinishedAt.UnixMicro(), r.RequestID, r.AttemptIndex, r.AppName, string(payload))
	if err != nil {
		return databaseError(err)
	}
	if r.Outcome == "cached" {
		hit := cachedEvent(r)
		payload, _ := json.Marshal(hit.Payload)
		if _, err = tx.ExecContext(ctx, `INSERT INTO events(event_id,type,occurred_at,request_id,attempt_index,app_name,payload_json) VALUES(?,'cached',?,?,?,?,?)`, hit.ID, r.FinishedAt.UnixMicro(), r.RequestID, r.AttemptIndex, r.AppName, string(payload)); err != nil {
			return databaseError(err)
		}
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM metering_journal WHERE request_id=? AND attempt_index=?`, r.RequestID, r.AttemptIndex)
	if err != nil {
		return databaseError(err)
	}
	*inserted = true
	return nil
}

const requestColumns = `request_id,attempt_index,started_at,finished_at,app_name,user_hash,anonymous,operation,provider,model_requested,model_used,upstream_status,downstream_status,outcome,error_class,error_code,input_tokens,output_tokens,cache_write_tokens,cache_read_tokens,raw_input_total,counted_input_tokens,cache_write_5m_tokens,cache_write_1h_tokens,cost_microusd,reserved_cost_microusd,usage_missing,recovered,estimate_exceeded,request_count,latency_us,upstream_ttfb_us,client_ttfb_us,usage_day,timezone,price_quote_json,record_json`

func insertRequest(ctx context.Context, tx *sql.Tx, r storage.Record, data, quote string) error {
	args := []any{r.RequestID, r.AttemptIndex, r.StartedAt.UnixMicro(), r.FinishedAt.UnixMicro(), r.AppName, r.UserHash, r.Anonymous, r.Operation, r.Provider, r.ModelRequested, r.ModelUsed, r.UpstreamStatus, r.DownstreamStatus, r.Outcome, r.ErrorClass, r.ErrorCode, r.Input, r.Output, r.CacheWrite, r.CacheRead, r.RawInputTotal, r.CountedInput, r.CacheWrite5m, r.CacheWrite1h, r.Cost, r.ReservedCost, r.UsageMissing, r.Recovered, r.EstimateExceeded, r.RequestCount, r.LatencyUS, r.UpstreamTTFB, r.ClientTTFB, r.Day, r.Timezone, quote, data}
	if _, err := tx.ExecContext(ctx, `INSERT INTO requests(`+requestColumns+`) VALUES(`+strings.TrimSuffix(strings.Repeat("?,", len(args)), ",")+`)`, args...); err != nil {
		return databaseError(err)
	}
	return nil
}

// commitBlocked stores a local refusal. It has no journal, no reservation and
// no daily aggregate: it is history and an event, never consumption.
func commitBlocked(ctx context.Context, tx *sql.Tx, r storage.Record, data, quote string, inserted *bool) error {
	if err := insertRequest(ctx, tx, r, data, quote); err != nil {
		return err
	}
	event := blockedEvent(r)
	payload, _ := json.Marshal(event.Payload)
	if _, err := tx.ExecContext(ctx, `INSERT INTO events(event_id,type,occurred_at,request_id,attempt_index,app_name,payload_json) VALUES(?,'blocked',?,?,0,?,?)`, event.ID, r.FinishedAt.UnixMicro(), r.RequestID, r.AppName, string(payload)); err != nil {
		return databaseError(err)
	}
	*inserted = true
	return nil
}

func (s *Store) Recover(ctx context.Context, now time.Time) error {
	pending, err := s.Pending(ctx)
	if err != nil {
		return err
	}
	batch := make([]storage.Record, 0, 1000)
	for _, p := range pending {
		r := p.Admission
		if p.Result != nil {
			r = *p.Result
			r.Recovered = true
		} else {
			r.FinishedAt = now.UTC()
			if r.FinishedAt.Before(r.StartedAt) {
				r.FinishedAt = r.StartedAt
			}
			r.Cost = r.ReservedCost
			r.UsageMissing = true
			r.Recovered = true
			if r.Outcome != "downgraded" && r.Outcome != "fallback" {
				r.Outcome = "ok"
			}
			r.RequestCount = 1
		}
		batch = append(batch, r)
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
