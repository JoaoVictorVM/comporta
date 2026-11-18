package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/JoaoVictorVM/comporta/internal/events"
	"github.com/JoaoVictorVM/comporta/internal/storage"
)

type operationRow struct {
	exists    bool
	state     string
	last      int
	app       string
	subject   string
	anonymous bool
	startedAt int64
}

func readOperation(ctx context.Context, tx *sql.Tx, requestID string) (operationRow, error) {
	var op operationRow
	err := tx.QueryRowContext(ctx, `SELECT state,last_attempt_index,app_name,subject,anonymous,started_at FROM request_operations WHERE request_id=$1 FOR UPDATE`, requestID).
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

// OperationFence carries the owner of a logical request into its session row.
type OperationFence struct {
	Epoch    string
	Revision int64
}

type fenceKey struct{}

// WithOperationFence attaches the cluster epoch and configuration revision a
// continuation was admitted under.
func WithOperationFence(ctx context.Context, f OperationFence) context.Context {
	return context.WithValue(ctx, fenceKey{}, f)
}

// AdmitContinuation admits a later attempt after the previous one was settled
// or released durably.
func (s *Store) AdmitContinuation(ctx context.Context, r storage.Record, res storage.Reservation, op storage.Operation) (int64, error) {
	if err := storage.ValidateRecord(r); err != nil {
		return 0, err
	}
	if err := storage.ValidReservation(res); err != nil {
		return 0, err
	}
	if r.AttemptIndex < 2 || r.AttemptIndex > storage.MaxAttempts || r.FallbackFrom == "" ||
		r.RequestID != res.RequestID || r.AttemptIndex != res.AttemptIndex || r.AppName != res.AppName || r.UserHash != res.Subject || r.Anonymous != res.Anonymous ||
		op.RequestID != r.RequestID || op.AppName != r.AppName || op.Subject != r.UserHash || op.Anonymous != r.Anonymous ||
		op.StartedAt.IsZero() || op.StartedAt.After(r.StartedAt) || op.Generation < 0 {
		return 0, storage.ErrInvalid
	}
	fence, _ := ctx.Value(fenceKey{}).(OperationFence)
	owner := s.currentOwner()
	var seq int64
	err := s.inTx(ctx, func(tx *sql.Tx) (func(), error) {
		current, err := readOperation(ctx, tx, r.RequestID)
		if err != nil {
			return nil, err
		}
		switch {
		case !current.exists && r.AttemptIndex == 2:
			if _, err = tx.ExecContext(ctx, `INSERT INTO request_operations(request_id,app_name,subject,anonymous,generation,started_at,state,last_attempt_index,owner_id,owner_fence,cluster_epoch,config_revision) VALUES($1,$2,$3,$4,$5,$6,'active',2,$7,$8,$9,$10)`,
				op.RequestID, op.AppName, op.Subject, op.Anonymous, op.Generation, micros(op.StartedAt), owner.ID, owner.Fence, fence.Epoch, fence.Revision); err != nil {
				return nil, databaseError(err)
			}
		case !current.exists:
			return nil, storage.ErrConflict
		case current.state != "active" || current.app != op.AppName || current.subject != op.Subject || current.anonymous != op.Anonymous:
			return nil, storage.ErrConflict
		case current.last == r.AttemptIndex-1:
			if _, err = tx.ExecContext(ctx, `UPDATE request_operations SET last_attempt_index=$1 WHERE request_id=$2 AND state='active'`, r.AttemptIndex, r.RequestID); err != nil {
				return nil, databaseError(err)
			}
		case current.last != r.AttemptIndex:
			return nil, storage.ErrConflict
		}
		if !current.exists || current.last != r.AttemptIndex {
			var previous string
			err = tx.QueryRowContext(ctx, `SELECT state FROM limit_reservations WHERE request_id=$1 AND attempt_index=$2`, r.RequestID, r.AttemptIndex-1).Scan(&previous)
			if errors.Is(err, sql.ErrNoRows) || (err == nil && previous == storage.ReservationReserved) {
				return nil, storage.ErrConflict
			}
			if err != nil {
				return nil, databaseError(err)
			}
		}
		if _, err = s.admitJournal(ctx, tx, r); err != nil {
			return nil, err
		}
		if seq, err = insertReservation(ctx, tx, res, owner.ID); err != nil {
			return nil, err
		}
		fallback := events.NewFallback(r.StartedAt, r.RequestID, r.AppName, r.AttemptIndex, r.FallbackFrom, r.ModelUsed, r.FallbackCause)
		if err = fallback.ValidateFallback(); err != nil {
			return nil, storage.ErrInvalid
		}
		candidates := []events.Event{fallback}
		if policy, ok := storage.PolicyEvent(r); ok {
			if err = policy.Validate(); err != nil {
				return nil, storage.ErrInvalid
			}
			candidates = append(candidates, policy)
		}
		var notify []events.Event
		for _, e := range candidates {
			payload, _ := json.Marshal(e.Payload)
			inserted, err := putEvent(ctx, tx, e, string(payload))
			if err != nil {
				return nil, err
			}
			if inserted {
				notify = append(notify, e)
			}
		}
		return func() {
			for _, e := range notify {
				s.Bus.Notify(e)
			}
		}, nil
	})
	return seq, err
}

func closeOperation(ctx context.Context, tx *sql.Tx, r storage.Record) error {
	code := r.TerminalErrorCode
	if code == "" {
		code = r.ErrorCode
	}
	status := r.TerminalStatus
	if status == nil {
		status = r.DownstreamStatus
	}
	result, err := tx.ExecContext(ctx, `UPDATE request_operations SET state='closed',terminal_attempt_index=$1,finished_at=greatest($2,started_at),terminal_status=$3,terminal_code=$4,materialized=true WHERE request_id=$5 AND state='active' AND last_attempt_index=$6`,
		r.AttemptIndex, micros(r.FinishedAt), status, code, r.RequestID, r.AttemptIndex)
	if err != nil {
		return databaseError(err)
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return storage.ErrConflict
	}
	return nil
}
