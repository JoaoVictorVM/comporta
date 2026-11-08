package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/events"
	"github.com/JoaoVictorVM/comporta/internal/storage"
	"io"
	"regexp"
	"strings"
	"time"
)

var hexID = regexp.MustCompile(`^[0-9a-f]{32}$`)

func validate(r storage.Record) error {
	if !hexID.MatchString(r.RequestID) || !hexID.MatchString(r.UserHash) || r.AppName == "" || r.AttemptIndex < 0 || r.Cost < 0 || r.ReservedCost < 0 || r.StartedAt.IsZero() || r.LatencyUS < 0 {
		return storage.ErrInvalid
	}
	if r.Operation != "generation" && r.Operation != "count_tokens" {
		return storage.ErrInvalid
	}
	switch r.Outcome {
	case "ok", "blocked", "cached", "fallback", "downgraded", "loop_cut":
	default:
		return storage.ErrInvalid
	}
	if _, err := time.LoadLocation(r.Timezone); err != nil {
		return storage.ErrInvalid
	}
	if _, err := time.Parse("2006-01-02", r.Day); err != nil {
		return storage.ErrInvalid
	}
	if r.RequestCount < 0 || r.RequestCount > 1 {
		return storage.ErrInvalid
	}
	if !r.FinishedAt.IsZero() && r.FinishedAt.Before(r.StartedAt) {
		return storage.ErrInvalid
	}
	if !recordName.MatchString(r.AppName) || len(r.ModelRequested) > 256 || len(r.ModelUsed) > 256 || !category.MatchString(r.ErrorClass) || !category.MatchString(r.ErrorCode) {
		return storage.ErrInvalid
	}
	for _, status := range []*int{r.UpstreamStatus, r.DownstreamStatus} {
		if status != nil && (*status < 100 || *status > 599) {
			return storage.ErrInvalid
		}
	}
	if r.Quote.Threshold < 0 || r.Quote.Write1h < 0 || r.Quote.HighWrite1h < 0 || r.Quote.EstimatedInput < 0 || r.Quote.EstimatedOutput < 0 || r.Quote.InputFormulaCost < 0 || r.Quote.ReservedCost < 0 {
		return storage.ErrInvalid
	}
	for _, p := range []config.Price{r.Quote.Price, r.Quote.HighPrice} {
		if p.Input < 0 || p.Output < 0 || p.CacheWrite < 0 || p.CacheRead < 0 {
			return storage.ErrInvalid
		}
	}
	switch r.Quote.Source {
	case "", "local", "override", "embedded":
	default:
		return storage.ErrInvalid
	}
	for _, n := range []*int64{r.Input, r.Output, r.CacheWrite, r.CacheRead, r.RawInputTotal, r.CountedInput, r.CacheWrite5m, r.CacheWrite1h, r.UpstreamTTFB, r.ClientTTFB} {
		if n != nil && *n < 0 {
			return storage.ErrInvalid
		}
	}
	return nil
}

var recordName = regexp.MustCompile(`^[a-z0-9-]{1,40}$`)
var category = regexp.MustCompile(`^[a-z0-9_]{0,64}$`)

func decodeRecord(data string, r *storage.Record) error {
	d := json.NewDecoder(bytes.NewBufferString(data))
	d.DisallowUnknownFields()
	if err := d.Decode(r); err != nil {
		return storage.ErrInvalid
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return storage.ErrInvalid
	}
	return validate(*r)
}

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
	data, _ := json.Marshal(r)
	var prior sql.NullString
	var metadata string
	if err := s.db.QueryRowContext(ctx, `SELECT metadata_json,result_json FROM metering_journal WHERE request_id=? AND attempt_index=?`, r.RequestID, r.AttemptIndex).Scan(&metadata, &prior); err != nil {
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
	_, err := s.db.ExecContext(ctx, `UPDATE metering_journal SET state='completed',result_json=? WHERE request_id=? AND attempt_index=?`, string(data), r.RequestID, r.AttemptIndex)
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

func sameAdmission(a, b storage.Record) bool {
	qa, _ := json.Marshal(a.Quote)
	qb, _ := json.Marshal(b.Quote)
	return a.RequestID == b.RequestID && a.AttemptIndex == b.AttemptIndex && a.AppName == b.AppName && a.UserHash == b.UserHash && a.Anonymous == b.Anonymous && a.Operation == b.Operation && a.Provider == b.Provider && a.ModelRequested == b.ModelRequested && a.ModelUsed == b.ModelUsed && a.StartedAt.Equal(b.StartedAt) && a.Day == b.Day && a.Timezone == b.Timezone && a.ReservedCost == b.ReservedCost && string(qa) == string(qb)
}

func (s *Store) Commit(ctx context.Context, records []storage.Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return databaseError(err)
	}
	defer tx.Rollback()
	var notifications []events.Event
	for _, r := range records {
		if err = validate(r); err != nil {
			return err
		}
		inserted := false
		if err = commitRecord(ctx, tx, r, &inserted); err != nil {
			return err
		}
		if inserted {
			notifications = append(notifications, events.Completion(r.RequestID, r.AttemptIndex, r.FinishedAt, r.AppName, r.Outcome, r.Cost, r.UsageMissing))
		}
	}
	if err = tx.Commit(); err != nil {
		return databaseError(err)
	}
	s.journalCount -= len(notifications)
	for _, e := range notifications {
		s.Bus.Notify(e)
	}
	return nil
}

func commitRecord(ctx context.Context, tx *sql.Tx, r storage.Record, inserted *bool) error {
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
		expected.Recovered = r.Recovered
		canonical, _ := json.Marshal(expected)
		if string(canonical) != string(data) {
			return storage.ErrConflict
		}
	}
	args := []any{r.RequestID, r.AttemptIndex, r.StartedAt.UnixMicro(), r.FinishedAt.UnixMicro(), r.AppName, r.UserHash, r.Anonymous, r.Operation, r.Provider, r.ModelRequested, r.ModelUsed, r.UpstreamStatus, r.DownstreamStatus, r.Outcome, r.ErrorClass, r.ErrorCode, r.Input, r.Output, r.CacheWrite, r.CacheRead, r.RawInputTotal, r.CountedInput, r.CacheWrite5m, r.CacheWrite1h, r.Cost, r.ReservedCost, r.UsageMissing, r.Recovered, r.EstimateExceeded, r.RequestCount, r.LatencyUS, r.UpstreamTTFB, r.ClientTTFB, r.Day, r.Timezone, string(quote), string(data)}
	_, err = tx.ExecContext(ctx, `INSERT INTO requests VALUES(`+strings.TrimSuffix(strings.Repeat("?,", len(args)), ",")+`)`, args...)
	if err != nil {
		return databaseError(err)
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
	event := events.Completion(r.RequestID, r.AttemptIndex, r.FinishedAt, r.AppName, r.Outcome, r.Cost, r.UsageMissing)
	payload, _ := json.Marshal(event.Payload)
	_, err = tx.ExecContext(ctx, `INSERT INTO events(event_id,type,occurred_at,request_id,attempt_index,app_name,payload_json) VALUES(?,'request_completed',?,?,?,?,?)`, event.ID, r.FinishedAt.UnixMicro(), r.RequestID, r.AttemptIndex, r.AppName, string(payload))
	if err != nil {
		return databaseError(err)
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM metering_journal WHERE request_id=? AND attempt_index=?`, r.RequestID, r.AttemptIndex)
	if err != nil {
		return databaseError(err)
	}
	*inserted = true
	return nil
}

func value(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func (s *Store) Recover(ctx context.Context, now time.Time) error {
	pending, err := s.Pending(ctx)
	if err != nil {
		return err
	}
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
			r.Outcome = "ok"
			r.RequestCount = 1
		}
		if err = s.Commit(ctx, []storage.Record{r}); err != nil {
			return err
		}
	}
	return nil
}
