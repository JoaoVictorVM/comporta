package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/JoaoVictorVM/comporta/internal/storage"
)

// CommitLocalCut writes the record of a request cut by the loop guard and the
// event of its episode in one transaction. Neither touches the journal, the
// daily aggregates or a reservation: a cut is history, never consumption.
func (s *Store) CommitLocalCut(ctx context.Context, cut storage.LocalCut) error {
	r, e := cut.Record, cut.Event
	if err := validate(r); err != nil {
		return err
	}
	if err := e.ValidateLoopCut(); err != nil {
		return storage.ErrInvalid
	}
	if r.Outcome != "loop_cut" || r.AttemptIndex != 0 || e.App != r.AppName || e.UserHash != r.UserHash || *e.Anonymous != r.Anonymous {
		return storage.ErrInvalid
	}
	data, _ := json.Marshal(r)
	quote, _ := json.Marshal(r.Quote)
	payload, err := json.Marshal(e.Payload)
	if err != nil {
		return storage.ErrInvalid
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
	inserted, err := putEvent(ctx, tx, e, string(payload))
	if err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return databaseError(err)
	}
	if inserted {
		s.Bus.Notify(e)
	}
	return nil
}
