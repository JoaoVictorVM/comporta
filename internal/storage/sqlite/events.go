package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/JoaoVictorVM/comporta/internal/events"
	"github.com/JoaoVictorVM/comporta/internal/storage"
)

func (s *Store) Publish(ctx context.Context, e events.Event) error {
	if err := e.Validate(); err != nil {
		return err
	}
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
	var previousType, previousPayload, previousRequest, previousApp string
	var previousTime int64
	err = tx.QueryRowContext(ctx, `SELECT type,payload_json,coalesce(request_id,''),coalesce(app_name,''),occurred_at FROM events WHERE event_id=?`, e.ID).Scan(&previousType, &previousPayload, &previousRequest, &previousApp, &previousTime)
	if err == nil {
		if previousType == string(e.Type) && previousPayload == string(payload) && previousRequest == e.RequestID && previousApp == e.App && previousTime == e.At.UnixMicro() {
			return nil
		}
		return storage.ErrConflict
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return databaseError(err)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO events(event_id,type,occurred_at,request_id,app_name,payload_json) VALUES(?,?,?,nullif(?,''),nullif(?,''),?)`, e.ID, string(e.Type), e.At.UnixMicro(), e.RequestID, e.App, string(payload))
	if err != nil {
		return databaseError(err)
	}
	if e.Type == events.ConfigChanged || e.Type == events.ConfigReloadFailed {
		changes, _ := json.Marshal(e.Payload.Changes)
		_, err = tx.ExecContext(ctx, `INSERT INTO config_changes VALUES(?,?,?,?)`, e.ID, e.Payload.Generation, string(e.Payload.Result), string(changes))
		if err != nil {
			return databaseError(err)
		}
	}
	if err = tx.Commit(); err != nil {
		return databaseError(err)
	}
	s.Bus.Notify(e)
	return nil
}
