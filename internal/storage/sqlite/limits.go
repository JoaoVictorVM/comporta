package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/storage"
)

type ledgerPayload struct {
	AppName string               `json:"app_name"`
	Deltas  []storage.LimitDelta `json:"deltas"`
}

func validDimension(d string) bool {
	switch d {
	case "usd_per_day", "usd_per_month", "tokens_per_day", "tokens_per_month":
		return true
	}
	return false
}

func validSubject(scope, subject string, anonymous bool) bool {
	switch scope {
	case "app":
		return subject == "" && !anonymous
	case "user":
		return hexID.MatchString(subject)
	}
	return false
}

func validReservation(r storage.Reservation) error {
	if !hexID.MatchString(r.RequestID) || r.AttemptIndex < 1 || !recordName.MatchString(r.AppName) || r.AdmittedAt.IsZero() || r.State != storage.ReservationReserved {
		return storage.ErrInvalid
	}
	if r.Subject != "" && !hexID.MatchString(r.Subject) {
		return storage.ErrInvalid
	}
	q := r.Quote
	if (q.Operation != "generation" && q.Operation != "count_tokens") || q.EstimatedTokens < 0 || q.ReservedCost < 0 {
		return storage.ErrInvalid
	}
	for _, item := range r.Items {
		if !validSubject(item.Scope, item.Subject, item.Anonymous) || !validDimension(item.Dimension) || item.WindowEnd <= item.WindowStart || item.Reserved < 0 || item.Settled != nil {
			return storage.ErrInvalid
		}
		if _, err := time.LoadLocation(item.Timezone); err != nil {
			return storage.ErrInvalid
		}
	}
	if e := r.Rate; e != nil {
		if e.RequestID != r.RequestID || e.AppName != r.AppName || e.Subject != r.Subject || e.Anonymous != r.Anonymous || e.EstimatedTokens < 0 || e.ActualTokens != nil || !e.Active || r.AttemptIndex != 1 {
			return storage.ErrInvalid
		}
	}
	return nil
}

func decodeStrict(data string, target any) error {
	d := json.NewDecoder(bytes.NewBufferString(data))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return storage.ErrInvalid
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return storage.ErrInvalid
	}
	return nil
}

// AdmitReserved writes the usage journal entry and the reservation in one
// transaction, so neither can exist without the other after a crash. A repeat
// with identical data returns the original sequence; different data is a
// conflict, never a second charge.
func (s *Store) AdmitReserved(ctx context.Context, r storage.Record, res storage.Reservation) (int64, error) {
	if err := validate(r); err != nil {
		return 0, err
	}
	if err := validReservation(res); err != nil {
		return 0, err
	}
	if r.RequestID != res.RequestID || r.AttemptIndex != res.AttemptIndex || r.AppName != res.AppName || r.UserHash != res.Subject || r.Anonymous != res.Anonymous {
		return 0, storage.ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, databaseError(err)
	}
	defer tx.Rollback()
	added, err := s.admitJournal(ctx, tx, r)
	if err != nil {
		return 0, err
	}
	seq, err := insertReservation(ctx, tx, res)
	if err != nil {
		return 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, databaseError(err)
	}
	if added {
		s.journalCount++
	}
	return seq, nil
}

func (s *Store) admitJournal(ctx context.Context, tx *sql.Tx, r storage.Record) (bool, error) {
	data, _ := json.Marshal(r)
	quote, _ := json.Marshal(r.Quote)
	var old string
	err := tx.QueryRowContext(ctx, `SELECT metadata_json FROM metering_journal WHERE request_id=? AND attempt_index=?`, r.RequestID, r.AttemptIndex).Scan(&old)
	if err == nil {
		if old == string(data) {
			return false, nil
		}
		return false, storage.ErrConflict
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, databaseError(err)
	}
	if s.journalCount >= 50000 {
		return false, storage.ErrUnavailable
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO metering_journal VALUES(?,?,?,'admitted',?,?,NULL)`, r.RequestID, r.AttemptIndex, r.StartedAt.UnixMicro(), string(data), string(quote)); err != nil {
		return false, databaseError(err)
	}
	return true, nil
}

func itemsJSON(items []storage.LimitItem) string {
	if items == nil {
		items = []storage.LimitItem{}
	}
	data, _ := json.Marshal(items)
	return string(data)
}

func loadItems(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, requestID string, attempt int) ([]storage.LimitItem, error) {
	rows, err := q.QueryContext(ctx, `SELECT scope,subject,anonymous,dimension,window_start,window_end,timezone,reserved,settled FROM limit_reservation_items WHERE request_id=? AND attempt_index=? ORDER BY item_index`, requestID, attempt)
	if err != nil {
		return nil, databaseError(err)
	}
	defer rows.Close()
	var items []storage.LimitItem
	for rows.Next() {
		var item storage.LimitItem
		var settled sql.NullInt64
		if err = rows.Scan(&item.Scope, &item.Subject, &item.Anonymous, &item.Dimension, &item.WindowStart, &item.WindowEnd, &item.Timezone, &item.Reserved, &settled); err != nil {
			return nil, databaseError(err)
		}
		if settled.Valid {
			value := settled.Int64
			item.Settled = &value
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func insertReservation(ctx context.Context, tx *sql.Tx, r storage.Reservation) (int64, error) {
	quote, _ := json.Marshal(r.Quote)
	var oldQuote, state string
	err := tx.QueryRowContext(ctx, `SELECT quote_json,state FROM limit_reservations WHERE request_id=? AND attempt_index=?`, r.RequestID, r.AttemptIndex).Scan(&oldQuote, &state)
	if err == nil {
		items, err := loadItems(ctx, tx, r.RequestID, r.AttemptIndex)
		if err != nil {
			return 0, err
		}
		for i := range items {
			items[i].Settled = nil
		}
		if oldQuote != string(quote) || itemsJSON(items) != itemsJSON(r.Items) {
			return 0, storage.ErrConflict
		}
		var seq int64
		if err = tx.QueryRowContext(ctx, `SELECT coalesce(max(seq),0) FROM limit_ledger WHERE request_id=? AND attempt_index=? AND transition='admit'`, r.RequestID, r.AttemptIndex).Scan(&seq); err != nil {
			return 0, databaseError(err)
		}
		return seq, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, databaseError(err)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO limit_reservations(request_id,attempt_index,app_name,subject,anonymous,state,admitted_at,finished_at,quote_json,settlement_json) VALUES(?,?,?,?,?,'reserved',?,NULL,?,NULL)`,
		r.RequestID, r.AttemptIndex, r.AppName, r.Subject, r.Anonymous, r.AdmittedAt.UnixMicro(), string(quote)); err != nil {
		return 0, databaseError(err)
	}
	payload := ledgerPayload{AppName: r.AppName, Deltas: []storage.LimitDelta{}}
	for i, item := range r.Items {
		if _, err = tx.ExecContext(ctx, `INSERT INTO limit_reservation_items VALUES(?,?,?,?,?,?,?,?,?,?,?,NULL)`,
			r.RequestID, r.AttemptIndex, i, item.Scope, item.Subject, item.Anonymous, item.Dimension, item.WindowStart, item.WindowEnd, item.Timezone, item.Reserved); err != nil {
			return 0, databaseError(err)
		}
		payload.Deltas = append(payload.Deltas, deltaFor(item, 0, item.Reserved))
	}
	if e := r.Rate; e != nil {
		var app, subject string
		var anonymous bool
		var admitted, estimated int64
		err = tx.QueryRowContext(ctx, `SELECT app_name,subject,anonymous,admitted_at,estimated_tokens FROM limit_rate_events WHERE request_id=?`, e.RequestID).Scan(&app, &subject, &anonymous, &admitted, &estimated)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if _, err = tx.ExecContext(ctx, `INSERT INTO limit_rate_events VALUES(?,?,?,?,?,?,NULL,1)`, e.RequestID, e.AppName, e.Subject, e.Anonymous, e.AdmittedAt.UnixMicro(), e.EstimatedTokens); err != nil {
				return 0, databaseError(err)
			}
		case err != nil:
			return 0, databaseError(err)
		case app != e.AppName || subject != e.Subject || anonymous != e.Anonymous || admitted != e.AdmittedAt.UnixMicro() || estimated != e.EstimatedTokens:
			return 0, storage.ErrConflict
		}
	}
	return insertLedger(ctx, tx, r.RequestID, r.AttemptIndex, storage.TransitionAdmit, r.AdmittedAt, payload)
}

func deltaFor(item storage.LimitItem, spent, reserved int64) storage.LimitDelta {
	return storage.LimitDelta{Scope: item.Scope, Subject: item.Subject, Anonymous: item.Anonymous, Dimension: item.Dimension, WindowStart: item.WindowStart, WindowEnd: item.WindowEnd, Spent: spent, Reserved: reserved}
}

func insertLedger(ctx context.Context, tx *sql.Tx, requestID string, attempt int, transition string, at time.Time, payload ledgerPayload) (int64, error) {
	data, _ := json.Marshal(payload)
	result, err := tx.ExecContext(ctx, `INSERT INTO limit_ledger(request_id,attempt_index,transition,occurred_at,payload_json) VALUES(?,?,?,?,?)`, requestID, attempt, transition, at.UnixMicro(), string(data))
	if err != nil {
		return 0, databaseError(err)
	}
	seq, err := result.LastInsertId()
	if err != nil {
		return 0, storage.ErrUnavailable
	}
	return seq, nil
}

// CompleteSettled makes the usage result and the settlement durable together.
// The later usage batch never settles the reservation a second time.
func (s *Store) CompleteSettled(ctx context.Context, r storage.Record, st storage.Settlement) (int64, error) {
	if err := validate(r); err != nil {
		return 0, err
	}
	if r.RequestID != st.RequestID || r.AttemptIndex != st.AttemptIndex {
		return 0, storage.ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, databaseError(err)
	}
	defer tx.Rollback()
	if err = completeOne(ctx, tx, r); err != nil {
		return 0, err
	}
	seq, err := settleTx(ctx, tx, st)
	if err != nil {
		return 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, databaseError(err)
	}
	return seq, nil
}

// SettleReservation ends a reservation on its own, for releases, retries and
// recovery.
func (s *Store) SettleReservation(ctx context.Context, st storage.Settlement) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, databaseError(err)
	}
	defer tx.Rollback()
	seq, err := settleTx(ctx, tx, st)
	if err != nil {
		return 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, databaseError(err)
	}
	return seq, nil
}

func settleTx(ctx context.Context, tx *sql.Tx, st storage.Settlement) (int64, error) {
	if !hexID.MatchString(st.RequestID) || st.AttemptIndex < 1 || st.At.IsZero() || (st.Transition != storage.TransitionSettle && st.Transition != storage.TransitionRelease) {
		return 0, storage.ErrInvalid
	}
	for _, n := range st.Settled {
		if n < 0 {
			return 0, storage.ErrInvalid
		}
	}
	if st.RateTokens != nil && *st.RateTokens < 0 {
		return 0, storage.ErrInvalid
	}
	settlement, _ := json.Marshal(st)
	var app, state string
	var admitted int64
	var previous sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT app_name,state,admitted_at,settlement_json FROM limit_reservations WHERE request_id=? AND attempt_index=?`, st.RequestID, st.AttemptIndex).Scan(&app, &state, &admitted, &previous)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, storage.ErrConflict
	}
	if err != nil {
		return 0, databaseError(err)
	}
	if state != storage.ReservationReserved {
		if !previous.Valid || previous.String != string(settlement) {
			return 0, storage.ErrConflict
		}
		var seq int64
		if err = tx.QueryRowContext(ctx, `SELECT coalesce(max(seq),0) FROM limit_ledger WHERE request_id=? AND attempt_index=? AND transition=?`, st.RequestID, st.AttemptIndex, st.Transition).Scan(&seq); err != nil {
			return 0, databaseError(err)
		}
		return seq, nil
	}
	items, err := loadItems(ctx, tx, st.RequestID, st.AttemptIndex)
	if err != nil {
		return 0, err
	}
	if len(items) != len(st.Settled) {
		return 0, storage.ErrInvalid
	}
	payload := ledgerPayload{AppName: app, Deltas: []storage.LimitDelta{}}
	for i, item := range items {
		if _, err = tx.ExecContext(ctx, `UPDATE limit_reservation_items SET settled=? WHERE request_id=? AND attempt_index=? AND item_index=?`, st.Settled[i], st.RequestID, st.AttemptIndex, i); err != nil {
			return 0, databaseError(err)
		}
		payload.Deltas = append(payload.Deltas, deltaFor(item, st.Settled[i], -item.Reserved))
	}
	next := storage.ReservationSettled
	if st.Transition == storage.TransitionRelease {
		next = storage.ReservationReleased
	}
	finished := max(st.At.UnixMicro(), admitted)
	if _, err = tx.ExecContext(ctx, `UPDATE limit_reservations SET state=?,finished_at=?,settlement_json=? WHERE request_id=? AND attempt_index=?`, next, finished, string(settlement), st.RequestID, st.AttemptIndex); err != nil {
		return 0, databaseError(err)
	}
	if st.RateTokens != nil && st.AttemptIndex == 1 {
		if _, err = tx.ExecContext(ctx, `UPDATE limit_rate_events SET actual_tokens=?,active=0 WHERE request_id=?`, *st.RateTokens, st.RequestID); err != nil {
			return 0, databaseError(err)
		}
	}
	return insertLedger(ctx, tx, st.RequestID, st.AttemptIndex, st.Transition, st.At, payload)
}

// LoadLimits reads everything recovery needs, in one read transaction.
func (s *Store) LoadLimits(ctx context.Context, rateSince time.Time) (storage.LimitState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var state storage.LimitState
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return state, databaseError(err)
	}
	defer tx.Rollback()
	var cp storage.LimitCheckpoint
	var created, watermark int64
	var metadata string
	err = tx.QueryRowContext(ctx, `SELECT covered_seq,created_at,civil_watermark,metadata_json FROM limit_checkpoints WHERE id=1`).Scan(&cp.CoveredSeq, &created, &watermark, &metadata)
	switch {
	case err == nil:
		cp.CreatedAt, cp.Watermark = time.UnixMicro(created).UTC(), time.UnixMicro(watermark).UTC()
		if err = decodeStrict(metadata, &cp.Metadata); err != nil || cp.Metadata.Version != 1 {
			return state, storage.ErrInvalid
		}
		if cp.Buckets, err = readBuckets(ctx, tx); err != nil {
			return state, err
		}
		state.Checkpoint = &cp
	case !errors.Is(err, sql.ErrNoRows):
		return state, databaseError(err)
	}
	if state.Ledger, err = readLedger(ctx, tx, cp.CoveredSeq); err != nil {
		return state, err
	}
	if state.Open, err = readOpen(ctx, tx); err != nil {
		return state, err
	}
	if state.RateEvents, err = readRates(ctx, tx, rateSince); err != nil {
		return state, err
	}
	if state.Checkpoint == nil {
		if state.Daily, err = readAllDaily(ctx, tx); err != nil {
			return state, err
		}
		if state.Missing, err = readMissing(ctx, tx); err != nil {
			return state, err
		}
	}
	return state, nil
}

func readBuckets(ctx context.Context, tx *sql.Tx) ([]storage.BudgetBucket, error) {
	rows, err := tx.QueryContext(ctx, `SELECT app_name,scope,subject,anonymous,dimension,window_start,window_end,spent,reserved FROM budget_state`)
	if err != nil {
		return nil, databaseError(err)
	}
	defer rows.Close()
	var out []storage.BudgetBucket
	for rows.Next() {
		var b storage.BudgetBucket
		if err = rows.Scan(&b.AppName, &b.Scope, &b.Subject, &b.Anonymous, &b.Dimension, &b.WindowStart, &b.WindowEnd, &b.Spent, &b.Reserved); err != nil {
			return nil, databaseError(err)
		}
		if !validDimension(b.Dimension) || !recordName.MatchString(b.AppName) {
			return nil, storage.ErrInvalid
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func readLedger(ctx context.Context, tx *sql.Tx, after int64) ([]storage.LedgerEntry, error) {
	rows, err := tx.QueryContext(ctx, `SELECT seq,request_id,attempt_index,transition,occurred_at,payload_json FROM limit_ledger WHERE seq>? ORDER BY seq`, after)
	if err != nil {
		return nil, databaseError(err)
	}
	defer rows.Close()
	var out []storage.LedgerEntry
	for rows.Next() {
		var e storage.LedgerEntry
		var at int64
		var data string
		if err = rows.Scan(&e.Seq, &e.RequestID, &e.AttemptIndex, &e.Transition, &at, &data); err != nil {
			return nil, databaseError(err)
		}
		var payload ledgerPayload
		if err = decodeStrict(data, &payload); err != nil || !recordName.MatchString(payload.AppName) {
			return nil, storage.ErrInvalid
		}
		for _, d := range payload.Deltas {
			if !validSubject(d.Scope, d.Subject, d.Anonymous) || !validDimension(d.Dimension) {
				return nil, storage.ErrInvalid
			}
		}
		e.OccurredAt, e.AppName, e.Deltas = time.UnixMicro(at).UTC(), payload.AppName, payload.Deltas
		out = append(out, e)
	}
	return out, rows.Err()
}

func readOpen(ctx context.Context, tx *sql.Tx) ([]storage.OpenReservation, error) {
	rows, err := tx.QueryContext(ctx, `SELECT request_id,attempt_index,app_name,subject,anonymous,admitted_at,quote_json FROM limit_reservations WHERE state='reserved' ORDER BY admitted_at,request_id,attempt_index`)
	if err != nil {
		return nil, databaseError(err)
	}
	var out []storage.OpenReservation
	for rows.Next() {
		var r storage.Reservation
		var admitted int64
		var quote string
		if err = rows.Scan(&r.RequestID, &r.AttemptIndex, &r.AppName, &r.Subject, &r.Anonymous, &admitted, &quote); err != nil {
			rows.Close()
			return nil, databaseError(err)
		}
		if err = decodeStrict(quote, &r.Quote); err != nil {
			rows.Close()
			return nil, err
		}
		r.AdmittedAt, r.State = time.UnixMicro(admitted).UTC(), storage.ReservationReserved
		out = append(out, storage.OpenReservation{Reservation: r})
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, databaseError(err)
	}
	for i := range out {
		r := &out[i].Reservation
		if r.Items, err = loadItems(ctx, tx, r.RequestID, r.AttemptIndex); err != nil {
			return nil, err
		}
		if out[i].Result, err = evidence(ctx, tx, r.RequestID, r.AttemptIndex); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// evidence is the best record of how an attempt ended: the exact result in the
// journal, else the flushed request. An admission without a result proves
// nothing beyond its reservation.
func evidence(ctx context.Context, tx *sql.Tx, requestID string, attempt int) (*storage.Record, error) {
	var result sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT result_json FROM metering_journal WHERE request_id=? AND attempt_index=?`, requestID, attempt).Scan(&result)
	if err == nil {
		if !result.Valid {
			return nil, nil
		}
		r := new(storage.Record)
		return r, decodeRecord(result.String, r)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, databaseError(err)
	}
	var data string
	err = tx.QueryRowContext(ctx, `SELECT record_json FROM requests WHERE request_id=? AND attempt_index=?`, requestID, attempt).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, databaseError(err)
	}
	r := new(storage.Record)
	return r, decodeRecord(data, r)
}

func readRates(ctx context.Context, tx *sql.Tx, since time.Time) ([]storage.RateEvent, error) {
	rows, err := tx.QueryContext(ctx, `SELECT request_id,app_name,subject,anonymous,admitted_at,estimated_tokens,actual_tokens,active FROM limit_rate_events WHERE active=1 OR admitted_at>? ORDER BY admitted_at,request_id`, since.UnixMicro())
	if err != nil {
		return nil, databaseError(err)
	}
	defer rows.Close()
	var out []storage.RateEvent
	for rows.Next() {
		var e storage.RateEvent
		var at int64
		var actual sql.NullInt64
		if err = rows.Scan(&e.RequestID, &e.AppName, &e.Subject, &e.Anonymous, &at, &e.EstimatedTokens, &actual, &e.Active); err != nil {
			return nil, databaseError(err)
		}
		e.AdmittedAt = time.UnixMicro(at).UTC()
		if actual.Valid {
			value := actual.Int64
			e.ActualTokens = &value
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func readAllDaily(ctx context.Context, tx *sql.Tx) ([]storage.Daily, error) {
	rows, err := tx.QueryContext(ctx, `SELECT day,timezone,app_name,scope,subject,anonymous,requests,attempts,input_tokens,output_tokens,cache_write_tokens,cache_read_tokens,usage_missing_count,recovered_count,cost_microusd FROM usage_daily ORDER BY day,timezone,app_name,scope,subject,anonymous`)
	if err != nil {
		return nil, databaseError(err)
	}
	defer rows.Close()
	var out []storage.Daily
	for rows.Next() {
		var d storage.Daily
		if err = rows.Scan(&d.Day, &d.Timezone, &d.AppName, &d.Scope, &d.Subject, &d.Anonymous, &d.Requests, &d.Attempts, &d.Input, &d.Output, &d.CacheWrite, &d.CacheRead, &d.UsageMissing, &d.Recovered, &d.Cost); err != nil {
			return nil, databaseError(err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// readMissing counts, per daily aggregate row, the retained records whose
// usage is unknown and the token estimate frozen in their quotes.
func readMissing(ctx context.Context, tx *sql.Tx) ([]storage.MissingUsage, error) {
	const estimate = `coalesce(json_extract(price_quote_json,'$.EstimatedInput'),0)+coalesce(json_extract(price_quote_json,'$.EstimatedOutput'),0)`
	var out []storage.MissingUsage
	for _, query := range []string{
		`SELECT usage_day,timezone,app_name,'app','',0,count(*),sum(` + estimate + `) FROM requests WHERE usage_missing=1 AND attempt_index>0 GROUP BY usage_day,timezone,app_name`,
		`SELECT usage_day,timezone,app_name,'user',user_hash,anonymous,count(*),sum(` + estimate + `) FROM requests WHERE usage_missing=1 AND attempt_index>0 AND user_hash IS NOT NULL GROUP BY usage_day,timezone,app_name,user_hash,anonymous`,
	} {
		rows, err := tx.QueryContext(ctx, query)
		if err != nil {
			return nil, databaseError(err)
		}
		for rows.Next() {
			var m storage.MissingUsage
			if err = rows.Scan(&m.Day, &m.Timezone, &m.AppName, &m.Scope, &m.Subject, &m.Anonymous, &m.Count, &m.EstimatedTokens); err != nil {
				rows.Close()
				return nil, databaseError(err)
			}
			out = append(out, m)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, databaseError(err)
		}
	}
	return out, nil
}

// SaveCheckpoint writes the changed projection rows, the covered sequence and
// the civil watermark in one transaction, then compacts what the checkpoint
// made redundant: ledger rows of finished reservations whose journal is gone,
// those reservations, and settled rate events outside the sliding window.
// Open reservations and anything a retry may still reach are kept.
func (s *Store) SaveCheckpoint(ctx context.Context, cp storage.LimitCheckpoint) error {
	if cp.CoveredSeq < 0 || cp.Metadata.Version != 1 {
		return storage.ErrInvalid
	}
	for _, b := range append(append([]storage.BudgetBucket(nil), cp.Buckets...), cp.Removed...) {
		if !recordName.MatchString(b.AppName) || !validSubject(b.Scope, b.Subject, b.Anonymous) || !validDimension(b.Dimension) || b.WindowEnd <= b.WindowStart || b.Spent < 0 || b.Reserved < 0 {
			return storage.ErrInvalid
		}
	}
	metadata, _ := json.Marshal(cp.Metadata)
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return databaseError(err)
	}
	defer tx.Rollback()
	now := cp.CreatedAt.UnixMicro()
	for _, b := range cp.Removed {
		if _, err = tx.ExecContext(ctx, `DELETE FROM budget_state WHERE app_name=? AND scope=? AND subject=? AND anonymous=? AND dimension=? AND window_start=? AND window_end=?`, b.AppName, b.Scope, b.Subject, b.Anonymous, b.Dimension, b.WindowStart, b.WindowEnd); err != nil {
			return databaseError(err)
		}
	}
	for _, b := range cp.Buckets {
		if _, err = tx.ExecContext(ctx, `INSERT INTO budget_state VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(app_name,scope,subject,anonymous,dimension,window_start,window_end) DO UPDATE SET spent=excluded.spent,reserved=excluded.reserved,updated_at=excluded.updated_at`,
			b.AppName, b.Scope, b.Subject, b.Anonymous, b.Dimension, b.WindowStart, b.WindowEnd, b.Spent, b.Reserved, now); err != nil {
			return databaseError(err)
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO limit_checkpoints VALUES(1,?,?,?,?) ON CONFLICT(id) DO UPDATE SET covered_seq=excluded.covered_seq,created_at=excluded.created_at,civil_watermark=excluded.civil_watermark,metadata_json=excluded.metadata_json`,
		cp.CoveredSeq, now, cp.Watermark.UnixMicro(), string(metadata)); err != nil {
		return databaseError(err)
	}
	const finished = `r.state<>'reserved' AND NOT EXISTS(SELECT 1 FROM metering_journal j WHERE j.request_id=r.request_id AND j.attempt_index=r.attempt_index)`
	const noLedger = `NOT EXISTS(SELECT 1 FROM limit_ledger l WHERE l.request_id=r.request_id AND l.attempt_index=r.attempt_index)`
	for _, step := range []struct {
		query string
		args  []any
	}{
		{`DELETE FROM limit_rate_events WHERE active=0 AND admitted_at<=?`, []any{cp.RateCutoff.UnixMicro()}},
		{`DELETE FROM limit_ledger WHERE seq<=? AND EXISTS(SELECT 1 FROM limit_reservations r WHERE r.request_id=limit_ledger.request_id AND r.attempt_index=limit_ledger.attempt_index AND ` + finished + `)`, []any{cp.CoveredSeq}},
		{`DELETE FROM limit_reservation_items WHERE EXISTS(SELECT 1 FROM limit_reservations r WHERE r.request_id=limit_reservation_items.request_id AND r.attempt_index=limit_reservation_items.attempt_index AND ` + finished + ` AND ` + noLedger + `)`, nil},
		{`DELETE FROM limit_reservations AS r WHERE ` + finished + ` AND ` + noLedger, nil},
	} {
		if _, err = tx.ExecContext(ctx, step.query, step.args...); err != nil {
			return databaseError(err)
		}
	}
	if err = tx.Commit(); err != nil {
		return databaseError(err)
	}
	return nil
}
