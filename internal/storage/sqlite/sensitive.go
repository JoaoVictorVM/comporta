package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/JoaoVictorVM/comporta/internal/storage"
)

const (
	codeSensitiveBlocked    = "sensitive_data_blocked"
	codeSensitiveScanFailed = "sensitive_data_scan_failed"
)

// CommitSensitiveCut writes the record of a request refused by the sensitive data
// inspection and, when the refusal came from a policy, the inspection event, in one
// transaction. Like a loop cut it never touches the journal, the daily aggregates or
// a reservation.
func (s *Store) CommitSensitiveCut(ctx context.Context, cut storage.SensitiveCut) error {
	r, e := cut.Record, cut.Event
	if err := validate(r); err != nil {
		return err
	}
	if r.Outcome != "blocked" || r.AttemptIndex != 0 {
		return storage.ErrInvalid
	}
	switch {
	case r.ErrorCode == codeSensitiveBlocked && e != nil:
		if e.ValidateSensitive() != nil || e.RequestID != r.RequestID || e.App != r.AppName || e.UserHash != r.UserHash || *e.Anonymous != r.Anonymous {
			return storage.ErrInvalid
		}
	case r.ErrorCode == codeSensitiveScanFailed && e == nil && r.Detections == nil:
	default:
		return storage.ErrInvalid
	}
	data, _ := json.Marshal(r)
	quote, _ := json.Marshal(r.Quote)
	var payload []byte
	if e != nil {
		var err error
		if payload, err = json.Marshal(e.Payload); err != nil {
			return storage.ErrInvalid
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return databaseError(err)
	}
	defer tx.Rollback()
	var old string
	err = tx.QueryRowContext(ctx, `SELECT record_json FROM requests WHERE request_id=? AND attempt_index=0`, r.RequestID).Scan(&old)
	switch {
	case err == nil:
		if old != string(data) {
			return storage.ErrConflict
		}
	case errors.Is(err, sql.ErrNoRows):
		if err = insertRequest(ctx, tx, r, string(data), string(quote)); err != nil {
			return err
		}
	default:
		return databaseError(err)
	}
	inserted := false
	if e != nil {
		if inserted, err = putEvent(ctx, tx, *e, string(payload)); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return databaseError(err)
	}
	if inserted {
		s.Bus.Notify(*e)
	}
	return nil
}
