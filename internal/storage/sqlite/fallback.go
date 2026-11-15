package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/JoaoVictorVM/comporta/internal/events"
	"github.com/JoaoVictorVM/comporta/internal/storage"
)

// operationRow is the durable session of a logical request that went past its
// first attempt. Requests answered by one attempt never get one.
type operationRow struct {
	exists    bool
	state     string
	last      int
	app       string
	subject   string
	anonymous bool
	startedAt int64
}

func hasTable(ctx context.Context, tx *sql.Tx, name string) (bool, error) {
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_schema WHERE type='table' AND name=?`, name).Scan(&n); err != nil {
		return false, databaseError(err)
	}
	return n > 0, nil
}

func readOperation(ctx context.Context, tx *sql.Tx, requestID string) (operationRow, error) {
	var op operationRow
	err := tx.QueryRowContext(ctx, `SELECT state,last_attempt_index,app_name,subject,anonymous,started_at FROM request_operations WHERE request_id=?`, requestID).
		Scan(&op.state, &op.last, &op.app, &op.subject, &op.anonymous, &op.startedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return op, nil
	}
	if err != nil {
		return op, databaseError(err)
	}
	op.exists = true
	return op, nil
}

// requestCount derives the role of an attempt from the session: only the last
// admitted attempt can be terminal. Without a session the record decides, as it
// always did for requests answered by one attempt.
func (op operationRow) requestCount(attempt, recorded int) (int, error) {
	if !op.exists {
		return recorded, nil
	}
	switch {
	case attempt < op.last:
		return 0, nil
	case attempt == op.last:
		return 1, nil
	}
	return 0, storage.ErrConflict
}

// AdmitContinuation admits a later attempt of a logical request. The previous
// attempt must already be settled or released, so its financial result is
// durable before anything else is sent.
func (s *Store) AdmitContinuation(ctx context.Context, r storage.Record, res storage.Reservation, op storage.Operation) (int64, error) {
	if err := validate(r); err != nil {
		return 0, err
	}
	if err := validReservation(res); err != nil {
		return 0, err
	}
	if r.AttemptIndex < 2 || r.AttemptIndex > storage.MaxAttempts || r.FallbackFrom == "" ||
		r.RequestID != res.RequestID || r.AttemptIndex != res.AttemptIndex || r.AppName != res.AppName || r.UserHash != res.Subject || r.Anonymous != res.Anonymous ||
		op.RequestID != r.RequestID || op.AppName != r.AppName || op.Subject != r.UserHash || op.Anonymous != r.Anonymous ||
		op.StartedAt.IsZero() || op.StartedAt.After(r.StartedAt) || op.Generation < 0 {
		return 0, storage.ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, databaseError(err)
	}
	defer tx.Rollback()
	current, err := readOperation(ctx, tx, r.RequestID)
	if err != nil {
		return 0, err
	}
	switch {
	case !current.exists && r.AttemptIndex == 2:
		if _, err = tx.ExecContext(ctx, `INSERT INTO request_operations(request_id,app_name,subject,anonymous,generation,started_at,state,last_attempt_index) VALUES(?,?,?,?,?,?,'active',2)`,
			op.RequestID, op.AppName, op.Subject, op.Anonymous, op.Generation, op.StartedAt.UnixMicro()); err != nil {
			return 0, databaseError(err)
		}
	case !current.exists:
		return 0, storage.ErrConflict
	case current.state != "active" || current.app != op.AppName || current.subject != op.Subject || current.anonymous != op.Anonymous:
		return 0, storage.ErrConflict
	case current.last == r.AttemptIndex-1:
		if _, err = tx.ExecContext(ctx, `UPDATE request_operations SET last_attempt_index=? WHERE request_id=? AND state='active'`, r.AttemptIndex, r.RequestID); err != nil {
			return 0, databaseError(err)
		}
	case current.last != r.AttemptIndex:
		return 0, storage.ErrConflict
	}
	// A repeat of the same admission finds the session already advanced; the
	// previous reservation may have been compacted by then.
	if !current.exists || current.last != r.AttemptIndex {
		var previous string
		err = tx.QueryRowContext(ctx, `SELECT state FROM limit_reservations WHERE request_id=? AND attempt_index=?`, r.RequestID, r.AttemptIndex-1).Scan(&previous)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && previous == storage.ReservationReserved) {
			return 0, storage.ErrConflict
		}
		if err != nil {
			return 0, databaseError(err)
		}
	}
	added, err := s.admitJournal(ctx, tx, r)
	if err != nil {
		return 0, err
	}
	seq, err := insertReservation(ctx, tx, res)
	if err != nil {
		return 0, err
	}
	var notify []events.Event
	fallback := events.NewFallback(r.StartedAt, r.RequestID, r.AppName, r.AttemptIndex, r.FallbackFrom, r.ModelUsed, r.FallbackCause)
	if err = fallback.ValidateFallback(); err != nil {
		return 0, storage.ErrInvalid
	}
	candidates := []events.Event{fallback}
	if policy, ok := policyEvent(r); ok {
		if err = policy.Validate(); err != nil {
			return 0, storage.ErrInvalid
		}
		candidates = append(candidates, policy)
	}
	for _, e := range candidates {
		payload, _ := json.Marshal(e.Payload)
		inserted, err := putEvent(ctx, tx, e, string(payload))
		if err != nil {
			return 0, err
		}
		if inserted {
			notify = append(notify, e)
		}
	}
	if err = tx.Commit(); err != nil {
		return 0, databaseError(err)
	}
	if added {
		s.journalCount++
	}
	for _, e := range notify {
		s.Bus.Notify(e)
	}
	return seq, nil
}

// closeOperation marks the session terminal at the attempt being materialized,
// together with its record and aggregates.
func closeOperation(ctx context.Context, tx *sql.Tx, r storage.Record) error {
	code := r.TerminalErrorCode
	if code == "" {
		code = r.ErrorCode
	}
	status := r.TerminalStatus
	if status == nil {
		status = r.DownstreamStatus
	}
	result, err := tx.ExecContext(ctx, `UPDATE request_operations SET state='closed',terminal_attempt_index=?,finished_at=max(?,started_at),terminal_status=?,terminal_code=?,materialized=1 WHERE request_id=? AND state='active' AND last_attempt_index=?`,
		r.AttemptIndex, r.FinishedAt.UnixMicro(), status, code, r.RequestID, r.AttemptIndex)
	if err != nil {
		return databaseError(err)
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return storage.ErrConflict
	}
	return nil
}
