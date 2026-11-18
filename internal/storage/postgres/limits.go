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

// AdmitReserved writes the journal entry, the reservation, its ledger delta,
// its rate event and the policy event in one transaction. A repeat with
// identical data returns the original sequence.
func (s *Store) AdmitReserved(ctx context.Context, r storage.Record, res storage.Reservation) (int64, error) {
	if err := storage.ValidateRecord(r); err != nil {
		return 0, err
	}
	if err := storage.ValidReservation(res); err != nil {
		return 0, err
	}
	if r.RequestID != res.RequestID || r.AttemptIndex != res.AttemptIndex || r.AppName != res.AppName || r.UserHash != res.Subject || r.Anonymous != res.Anonymous {
		return 0, storage.ErrInvalid
	}
	var seq int64
	err := s.inTx(ctx, func(tx *sql.Tx) (func(), error) {
		if _, err := s.admitJournal(ctx, tx, r); err != nil {
			return nil, err
		}
		var err error
		if seq, err = insertReservation(ctx, tx, res); err != nil {
			return nil, err
		}
		event, hasEvent := storage.PolicyEvent(r)
		if !hasEvent {
			return nil, nil
		}
		if err = event.Validate(); err != nil {
			return nil, storage.ErrInvalid
		}
		payload, _ := json.Marshal(event.Payload)
		inserted, err := putEvent(ctx, tx, event, string(payload))
		if err != nil || !inserted {
			return nil, err
		}
		return func() { s.Bus.Notify(event) }, nil
	})
	return seq, err
}

func loadIndexedItems(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, requestID string, attempt int) ([]storage.LimitItem, []int, error) {
	rows, err := q.QueryContext(ctx, `SELECT item_index,scope,subject,anonymous,dimension,window_start,window_end,timezone,reserved,settled FROM limit_reservation_items WHERE request_id=$1 AND attempt_index=$2 ORDER BY item_index`, requestID, attempt)
	if err != nil {
		return nil, nil, databaseError(err)
	}
	defer rows.Close()
	var items []storage.LimitItem
	var indices []int
	for rows.Next() {
		var item storage.LimitItem
		var index int
		var settled sql.NullInt64
		if err = rows.Scan(&index, &item.Scope, &item.Subject, &item.Anonymous, &item.Dimension, &item.WindowStart, &item.WindowEnd, &item.Timezone, &item.Reserved, &settled); err != nil {
			return nil, nil, databaseError(err)
		}
		if settled.Valid {
			v := settled.Int64
			item.Settled = &v
		}
		items = append(items, item)
		indices = append(indices, index)
	}
	return items, indices, rows.Err()
}

func loadItems(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, requestID string, attempt int) ([]storage.LimitItem, error) {
	items, _, err := loadIndexedItems(ctx, q, requestID, attempt)
	return items, err
}

func canonicalLimitQuote(text string) (string, error) {
	var q storage.LimitQuote
	if err := storage.DecodeStrict(text, &q); err != nil {
		return "", err
	}
	data, _ := json.Marshal(q)
	return string(data), nil
}

func insertReservation(ctx context.Context, tx *sql.Tx, r storage.Reservation) (int64, error) {
	quote, _ := json.Marshal(r.Quote)
	var oldQuote, state string
	err := tx.QueryRowContext(ctx, `SELECT quote_json::text,state FROM limit_reservations WHERE request_id=$1 AND attempt_index=$2`, r.RequestID, r.AttemptIndex).Scan(&oldQuote, &state)
	if err == nil {
		items, err := loadItems(ctx, tx, r.RequestID, r.AttemptIndex)
		if err != nil {
			return 0, err
		}
		for i := range items {
			items[i].Settled = nil
		}
		canonical, err := canonicalLimitQuote(oldQuote)
		if err != nil {
			return 0, err
		}
		if canonical != string(quote) || storage.ItemsJSON(items) != storage.ItemsJSON(r.Items) {
			return 0, storage.ErrConflict
		}
		var seq int64
		if err = tx.QueryRowContext(ctx, `SELECT coalesce(max(seq),0) FROM limit_ledger WHERE request_id=$1 AND attempt_index=$2 AND transition='admit'`, r.RequestID, r.AttemptIndex).Scan(&seq); err != nil {
			return 0, databaseError(err)
		}
		return seq, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, databaseError(err)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO limit_reservations(request_id,attempt_index,app_name,subject,anonymous,state,admitted_at,finished_at,quote_json,settlement_json) VALUES($1,$2,$3,$4,$5,'reserved',$6,NULL,$7,NULL)`,
		r.RequestID, r.AttemptIndex, r.AppName, r.Subject, r.Anonymous, micros(r.AdmittedAt), string(quote)); err != nil {
		return 0, databaseError(err)
	}
	payload := storage.LedgerPayload{AppName: r.AppName, Deltas: []storage.LimitDelta{}}
	if len(r.Items) > 0 {
		var values []string
		var args []any
		for i, item := range r.Items {
			values = append(values, "("+placeholdersFrom(len(args)+1, 11)+",NULL)")
			args = append(args, r.RequestID, r.AttemptIndex, i, item.Scope, item.Subject, item.Anonymous, item.Dimension, item.WindowStart, item.WindowEnd, item.Timezone, item.Reserved)
			payload.Deltas = append(payload.Deltas, storage.DeltaFor(item, 0, item.Reserved))
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO limit_reservation_items VALUES `+strings.Join(values, ","), args...); err != nil {
			return 0, databaseError(err)
		}
	}
	if e := r.Rate; e != nil && r.AttemptIndex > 1 {
		if _, err = tx.ExecContext(ctx, `INSERT INTO limit_attempt_rate_events VALUES($1,$2,$3,$4,$5,$6,$7,NULL,true)`, e.RequestID, r.AttemptIndex, e.AppName, e.Subject, e.Anonymous, micros(e.AdmittedAt), e.EstimatedTokens); err != nil {
			return 0, databaseError(err)
		}
	} else if e != nil {
		var app, subject string
		var anonymous bool
		var admitted, estimated int64
		err = tx.QueryRowContext(ctx, `SELECT app_name,subject,anonymous,admitted_at,estimated_tokens FROM limit_rate_events WHERE request_id=$1`, e.RequestID).Scan(&app, &subject, &anonymous, &admitted, &estimated)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if _, err = tx.ExecContext(ctx, `INSERT INTO limit_rate_events VALUES($1,$2,$3,$4,$5,$6,NULL,true)`, e.RequestID, e.AppName, e.Subject, e.Anonymous, micros(e.AdmittedAt), e.EstimatedTokens); err != nil {
				return 0, databaseError(err)
			}
		case err != nil:
			return 0, databaseError(err)
		case app != e.AppName || subject != e.Subject || anonymous != e.Anonymous || admitted != micros(e.AdmittedAt) || estimated != e.EstimatedTokens:
			return 0, storage.ErrConflict
		}
	}
	return insertLedger(ctx, tx, r.RequestID, r.AttemptIndex, storage.TransitionAdmit, r.AdmittedAt, payload)
}

func insertLedger(ctx context.Context, tx *sql.Tx, requestID string, attempt int, transition string, at time.Time, payload storage.LedgerPayload) (int64, error) {
	data, _ := json.Marshal(payload)
	var seq int64
	if err := tx.QueryRowContext(ctx, `INSERT INTO limit_ledger(request_id,attempt_index,transition,occurred_at,payload_json) VALUES($1,$2,$3,$4,$5) RETURNING seq`,
		requestID, attempt, transition, micros(at), string(data)).Scan(&seq); err != nil {
		return 0, databaseError(err)
	}
	return seq, nil
}

// CompleteSettled makes the result and the settlement durable together.
func (s *Store) CompleteSettled(ctx context.Context, r storage.Record, st storage.Settlement) (int64, error) {
	if err := storage.ValidateRecord(r); err != nil {
		return 0, err
	}
	if r.RequestID != st.RequestID || r.AttemptIndex != st.AttemptIndex {
		return 0, storage.ErrInvalid
	}
	var seq int64
	err := s.inTx(ctx, func(tx *sql.Tx) (func(), error) {
		if err := completeOne(ctx, tx, r); err != nil {
			return nil, err
		}
		var err error
		seq, err = settleTx(ctx, tx, st)
		return nil, err
	})
	return seq, err
}

// SettleReservation ends a reservation on its own.
func (s *Store) SettleReservation(ctx context.Context, st storage.Settlement) (int64, error) {
	var seq int64
	err := s.inTx(ctx, func(tx *sql.Tx) (func(), error) {
		var err error
		seq, err = settleTx(ctx, tx, st)
		return nil, err
	})
	return seq, err
}

func canonicalSettlement(text string) (string, error) {
	var st storage.Settlement
	if err := storage.DecodeStrict(text, &st); err != nil {
		return "", err
	}
	data, _ := json.Marshal(st)
	return string(data), nil
}

func settleTx(ctx context.Context, tx *sql.Tx, st storage.Settlement) (int64, error) {
	if !storage.HexID.MatchString(st.RequestID) || st.AttemptIndex < 1 || st.At.IsZero() || (st.Transition != storage.TransitionSettle && st.Transition != storage.TransitionRelease) {
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
	err := tx.QueryRowContext(ctx, `SELECT app_name,state,admitted_at,settlement_json::text FROM limit_reservations WHERE request_id=$1 AND attempt_index=$2 FOR UPDATE`, st.RequestID, st.AttemptIndex).Scan(&app, &state, &admitted, &previous)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, storage.ErrConflict
	}
	if err != nil {
		return 0, databaseError(err)
	}
	if state != storage.ReservationReserved {
		if !previous.Valid {
			return 0, storage.ErrConflict
		}
		canonical, err := canonicalSettlement(previous.String)
		if err != nil || canonical != string(settlement) {
			return 0, storage.ErrConflict
		}
		var seq int64
		if err = tx.QueryRowContext(ctx, `SELECT coalesce(max(seq),0) FROM limit_ledger WHERE request_id=$1 AND attempt_index=$2 AND transition=$3`, st.RequestID, st.AttemptIndex, st.Transition).Scan(&seq); err != nil {
			return 0, databaseError(err)
		}
		return seq, nil
	}
	items, indices, err := loadIndexedItems(ctx, tx, st.RequestID, st.AttemptIndex)
	if err != nil {
		return 0, err
	}
	if len(items) != len(st.Settled) {
		return 0, storage.ErrInvalid
	}
	payload := storage.LedgerPayload{AppName: app, Deltas: []storage.LimitDelta{}}
	if len(items) > 0 {
		var values []string
		args := []any{st.RequestID, st.AttemptIndex}
		for i, item := range items {
			n := len(args)
			values = append(values, "($"+strconv.Itoa(n+1)+"::integer,$"+strconv.Itoa(n+2)+"::bigint)")
			args = append(args, indices[i], st.Settled[i])
			payload.Deltas = append(payload.Deltas, storage.DeltaFor(item, st.Settled[i], -item.Reserved))
		}
		if _, err = tx.ExecContext(ctx, `UPDATE limit_reservation_items i SET settled=v.settled FROM (VALUES `+strings.Join(values, ",")+`) AS v(item_index,settled)
WHERE i.request_id=$1 AND i.attempt_index=$2 AND i.item_index=v.item_index`, args...); err != nil {
			return 0, databaseError(err)
		}
	}
	next := storage.ReservationSettled
	if st.Transition == storage.TransitionRelease {
		next = storage.ReservationReleased
	}
	finished := max(micros(st.At), admitted)
	if _, err = tx.ExecContext(ctx, `UPDATE limit_reservations SET state=$1,finished_at=$2,settlement_json=$3 WHERE request_id=$4 AND attempt_index=$5`, next, finished, string(settlement), st.RequestID, st.AttemptIndex); err != nil {
		return 0, databaseError(err)
	}
	if st.RateTokens != nil && st.AttemptIndex == 1 {
		if _, err = tx.ExecContext(ctx, `UPDATE limit_rate_events SET actual_tokens=$1,active=false WHERE request_id=$2`, *st.RateTokens, st.RequestID); err != nil {
			return 0, databaseError(err)
		}
	} else if st.RateTokens != nil {
		if _, err = tx.ExecContext(ctx, `UPDATE limit_attempt_rate_events SET actual_tokens=$1,active=false WHERE request_id=$2 AND attempt_index=$3`, *st.RateTokens, st.RequestID, st.AttemptIndex); err != nil {
			return 0, databaseError(err)
		}
	}
	return insertLedger(ctx, tx, st.RequestID, st.AttemptIndex, st.Transition, st.At, payload)
}

// LoadReservation reads one reservation with its items, for a settlement
// replayed or recovered by another process.
func (s *Store) LoadReservation(ctx context.Context, requestID string, attempt int) (storage.Reservation, error) {
	var r storage.Reservation
	var admitted int64
	var quote string
	err := s.db.QueryRowContext(ctx, `SELECT request_id,attempt_index,app_name,subject,anonymous,state,admitted_at,quote_json::text FROM limit_reservations WHERE request_id=$1 AND attempt_index=$2`, requestID, attempt).
		Scan(&r.RequestID, &r.AttemptIndex, &r.AppName, &r.Subject, &r.Anonymous, &r.State, &admitted, &quote)
	if errors.Is(err, sql.ErrNoRows) {
		return r, storage.ErrConflict
	}
	if err != nil {
		return r, databaseError(err)
	}
	if err = storage.DecodeStrict(quote, &r.Quote); err != nil {
		return r, err
	}
	r.AdmittedAt = time.UnixMicro(admitted).UTC()
	if r.Items, err = loadItems(ctx, s.db, requestID, attempt); err != nil {
		return r, err
	}
	for i := range r.Items {
		r.Items[i].Settled = nil
	}
	return r, nil
}

// LoadLimits reads everything a rebuild needs in one repeatable-read
// transaction, so the checkpoint, the ledger and the open reservations are one
// consistent state.
func (s *Store) LoadLimits(ctx context.Context, rateSince time.Time) (storage.LimitState, error) {
	var state storage.LimitState
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return state, databaseError(err)
	}
	defer tx.Rollback()
	var cp storage.LimitCheckpoint
	var created, watermark int64
	var metadata string
	err = tx.QueryRowContext(ctx, `SELECT covered_seq,created_at,civil_watermark,metadata_json::text FROM limit_checkpoints WHERE id=1`).Scan(&cp.CoveredSeq, &created, &watermark, &metadata)
	switch {
	case err == nil:
		cp.CreatedAt, cp.Watermark = time.UnixMicro(created).UTC(), time.UnixMicro(watermark).UTC()
		if err = storage.DecodeStrict(metadata, &cp.Metadata); err != nil || cp.Metadata.Version != 1 {
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
		out = append(out, b)
	}
	return out, rows.Err()
}

func readLedger(ctx context.Context, tx *sql.Tx, after int64) ([]storage.LedgerEntry, error) {
	rows, err := tx.QueryContext(ctx, `SELECT seq,request_id,attempt_index,transition,occurred_at,payload_json::text FROM limit_ledger WHERE seq>$1 ORDER BY seq`, after)
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
		var payload storage.LedgerPayload
		if err = storage.DecodeStrict(data, &payload); err != nil || !storage.RecordName.MatchString(payload.AppName) {
			return nil, storage.ErrInvalid
		}
		for _, d := range payload.Deltas {
			if !storage.ValidSubject(d.Scope, d.Subject, d.Anonymous) || !storage.ValidDimension(d.Dimension) {
				return nil, storage.ErrInvalid
			}
		}
		e.OccurredAt, e.AppName, e.Deltas = time.UnixMicro(at).UTC(), payload.AppName, payload.Deltas
		out = append(out, e)
	}
	return out, rows.Err()
}

func readOpen(ctx context.Context, tx *sql.Tx) ([]storage.OpenReservation, error) {
	rows, err := tx.QueryContext(ctx, `SELECT request_id,attempt_index,app_name,subject,anonymous,admitted_at,quote_json::text FROM limit_reservations WHERE state='reserved' ORDER BY admitted_at,request_id,attempt_index`)
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
		if err = storage.DecodeStrict(quote, &r.Quote); err != nil {
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

func evidence(ctx context.Context, tx *sql.Tx, requestID string, attempt int) (*storage.Record, error) {
	var result sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT result_json::text FROM metering_journal WHERE request_id=$1 AND attempt_index=$2`, requestID, attempt).Scan(&result)
	if err == nil {
		if !result.Valid {
			return nil, nil
		}
		_, r, err := canonicalRecord(result.String)
		return &r, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, databaseError(err)
	}
	var data string
	err = tx.QueryRowContext(ctx, `SELECT record_json::text FROM requests WHERE request_id=$1 AND attempt_index=$2`, requestID, attempt).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, databaseError(err)
	}
	_, r, err := canonicalRecord(data)
	return &r, err
}

func readRates(ctx context.Context, tx *sql.Tx, since time.Time) ([]storage.RateEvent, error) {
	out, err := readRateTable(ctx, tx, `SELECT request_id,1,app_name,subject,anonymous,admitted_at,estimated_tokens,actual_tokens,active FROM limit_rate_events WHERE active OR admitted_at>$1 ORDER BY admitted_at,request_id`, since)
	if err != nil {
		return nil, err
	}
	later, err := readRateTable(ctx, tx, `SELECT request_id,attempt_index,app_name,subject,anonymous,admitted_at,estimated_tokens,actual_tokens,active FROM limit_attempt_rate_events WHERE active OR admitted_at>$1 ORDER BY admitted_at,request_id,attempt_index`, since)
	if err != nil {
		return nil, err
	}
	return append(out, later...), nil
}

func readRateTable(ctx context.Context, tx *sql.Tx, query string, since time.Time) ([]storage.RateEvent, error) {
	rows, err := tx.QueryContext(ctx, query, micros(since))
	if err != nil {
		return nil, databaseError(err)
	}
	defer rows.Close()
	var out []storage.RateEvent
	for rows.Next() {
		var e storage.RateEvent
		var at int64
		var actual sql.NullInt64
		if err = rows.Scan(&e.RequestID, &e.AttemptIndex, &e.AppName, &e.Subject, &e.Anonymous, &at, &e.EstimatedTokens, &actual, &e.Active); err != nil {
			return nil, databaseError(err)
		}
		e.AdmittedAt = time.UnixMicro(at).UTC()
		if actual.Valid {
			v := actual.Int64
			e.ActualTokens = &v
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// SaveCheckpoint is not how the shared projection is checkpointed: the ledger
// is folded in SQL under the exclusive gate by FoldCheckpoint, since no
// process holds the whole projection in memory.
func (s *Store) SaveCheckpoint(context.Context, storage.LimitCheckpoint) error {
	return storage.ErrInvalid
}

// FoldCheckpoint applies every ledger delta committed since the last
// checkpoint to budget_state and advances the covered sequence. The caller
// holds the exclusive gate in the same transaction, so no admission or
// settlement can commit a lower sequence afterwards and MAX(seq) is a
// complete prefix. Settled rate events outside the sliding window, finished
// reservations and their covered ledger rows are compacted, never open ones.
func FoldCheckpoint(ctx context.Context, tx *sql.Tx, at time.Time, zones map[string]storage.ZoneState, unknown []storage.BucketRef) (int64, error) {
	var covered int64
	err := tx.QueryRowContext(ctx, `SELECT covered_seq FROM limit_checkpoints WHERE id=1`).Scan(&covered)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, databaseError(err)
	}
	var top int64
	if err = tx.QueryRowContext(ctx, `SELECT coalesce(max(seq),0) FROM limit_ledger`).Scan(&top); err != nil {
		return 0, databaseError(err)
	}
	top = max(top, covered)
	entries, err := readLedger(ctx, tx, covered)
	if err != nil {
		return 0, err
	}
	now := micros(at)
	for _, e := range entries {
		if e.Seq > top {
			break
		}
		for _, d := range e.Deltas {
			if _, err = tx.ExecContext(ctx, `INSERT INTO budget_state VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
ON CONFLICT(app_name,scope,subject,anonymous,dimension,window_start,window_end) DO UPDATE SET spent=budget_state.spent+excluded.spent,reserved=budget_state.reserved+excluded.reserved,updated_at=excluded.updated_at`,
				e.AppName, d.Scope, d.Subject, d.Anonymous, d.Dimension, d.WindowStart, d.WindowEnd, d.Spent, d.Reserved, now); err != nil {
				return 0, databaseError(err)
			}
		}
	}
	if zones == nil {
		zones = map[string]storage.ZoneState{}
	}
	metadata, _ := json.Marshal(storage.CheckpointMetadata{Version: 1, Zones: zones, Unknown: unknown})
	if _, err = tx.ExecContext(ctx, `INSERT INTO limit_checkpoints VALUES(1,$1,$2,$3,$4) ON CONFLICT(id) DO UPDATE SET covered_seq=excluded.covered_seq,created_at=excluded.created_at,civil_watermark=excluded.civil_watermark,metadata_json=excluded.metadata_json`,
		top, now, now, string(metadata)); err != nil {
		return 0, databaseError(err)
	}
	cutoff := micros(at.Add(-time.Minute))
	const finished = `r.state<>'reserved' AND NOT EXISTS(SELECT 1 FROM metering_journal j WHERE j.request_id=r.request_id AND j.attempt_index=r.attempt_index)
AND NOT EXISTS(SELECT 1 FROM coordination_outbox o WHERE o.operation_id=r.request_id||':'||r.attempt_index AND o.delivered_at IS NULL)`
	const noLedger = `NOT EXISTS(SELECT 1 FROM limit_ledger l WHERE l.request_id=r.request_id AND l.attempt_index=r.attempt_index)`
	for _, step := range []struct {
		query string
		args  []any
	}{
		{`DELETE FROM budget_state WHERE window_end<=$1 AND reserved=0`, []any{now}},
		{`DELETE FROM limit_attempt_rate_events WHERE NOT active AND admitted_at<=$1`, []any{cutoff}},
		{`DELETE FROM limit_rate_events WHERE NOT active AND admitted_at<=$1`, []any{cutoff}},
		{`DELETE FROM limit_ledger WHERE seq<=$1 AND EXISTS(SELECT 1 FROM limit_reservations r WHERE r.request_id=limit_ledger.request_id AND r.attempt_index=limit_ledger.attempt_index AND ` + finished + `)`, []any{top}},
		{`DELETE FROM limit_reservation_items WHERE EXISTS(SELECT 1 FROM limit_reservations r WHERE r.request_id=limit_reservation_items.request_id AND r.attempt_index=limit_reservation_items.attempt_index AND ` + finished + ` AND ` + noLedger + `)`, nil},
		{`DELETE FROM limit_reservations AS r WHERE ` + finished + ` AND ` + noLedger, nil},
	} {
		if _, err = tx.ExecContext(ctx, step.query, step.args...); err != nil {
			return 0, databaseError(err)
		}
	}
	return top, nil
}

func putEvent(ctx context.Context, tx *sql.Tx, e events.Event, payload string) (bool, error) {
	var previousType, previousPayload, previousRequest, previousApp, previousHash string
	var previousTime int64
	var previousAnonymous sql.NullBool
	err := tx.QueryRowContext(ctx, `SELECT type,payload_json::text,coalesce(request_id,''),coalesce(app_name,''),coalesce(user_hash,''),anonymous,occurred_at FROM events WHERE event_id=$1`, e.ID).
		Scan(&previousType, &previousPayload, &previousRequest, &previousApp, &previousHash, &previousAnonymous, &previousTime)
	if err == nil {
		var stored, incoming events.Payload
		if json.Unmarshal([]byte(previousPayload), &stored) != nil || json.Unmarshal([]byte(payload), &incoming) != nil {
			return false, storage.ErrInvalid
		}
		a, _ := json.Marshal(stored)
		b, _ := json.Marshal(incoming)
		sameSubject := previousHash == e.UserHash && previousAnonymous.Valid == (e.Anonymous != nil) && (e.Anonymous == nil || previousAnonymous.Bool == *e.Anonymous)
		if previousType == string(e.Type) && string(a) == string(b) && previousRequest == e.RequestID && previousApp == e.App && previousTime == micros(e.At) && sameSubject {
			return false, nil
		}
		return false, storage.ErrConflict
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, databaseError(err)
	}
	var anonymous any
	if e.Anonymous != nil {
		anonymous = *e.Anonymous
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO events(event_id,type,occurred_at,request_id,app_name,user_hash,anonymous,payload_json) VALUES($1,$2,$3,nullif($4,''),nullif($5,''),nullif($6,''),$7,$8)`,
		e.ID, string(e.Type), micros(e.At), e.RequestID, e.App, e.UserHash, anonymous, payload); err != nil {
		return false, databaseError(err)
	}
	return true, nil
}
