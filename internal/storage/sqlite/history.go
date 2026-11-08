package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/JoaoVictorVM/comporta/internal/events"
	"github.com/JoaoVictorVM/comporta/internal/storage"
	"github.com/JoaoVictorVM/comporta/internal/useridentity"
	"os"
	"time"
)

func (s *Store) Inspect(ctx context.Context) (useridentity.HistoryState, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return useridentity.HistoryPresent, storage.ErrUnavailable
	}
	allowed := map[string]bool{"keys.json": true, "keys.lock": true, "hmac.key": true, "hmac.lock": true, "usage.lock": true, "usage.sqlite": true, "usage.sqlite-wal": true, "usage.sqlite-shm": true}
	for _, e := range entries {
		if !allowed[e.Name()] || !e.Type().IsRegular() {
			return useridentity.HistoryPresent, nil
		}
	}
	if s.db == nil {
		for _, e := range entries {
			if e.Name() == "usage.sqlite-wal" || e.Name() == "usage.sqlite-shm" {
				return useridentity.HistoryPresent, storage.ErrUnavailable
			}
		}
		return useridentity.HistoryEmpty, nil
	}
	for _, query := range []string{`SELECT count(*) FROM requests WHERE user_hash IS NOT NULL`, `SELECT count(*) FROM usage_daily WHERE scope='user'`, `SELECT count(*) FROM metering_journal`, `SELECT count(*) FROM budget_state WHERE scope='user'`, `SELECT count(*) FROM events WHERE user_hash IS NOT NULL`} {
		var n int
		if err = s.db.QueryRowContext(ctx, query).Scan(&n); err != nil {
			return useridentity.HistoryPresent, storage.ErrUnavailable
		}
		if n > 0 {
			return useridentity.HistoryPresent, nil
		}
	}
	return useridentity.HistoryEmpty, nil
}

func (s *Store) Reset(ctx context.Context) error {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return storage.ErrUnavailable
	}
	allowed := map[string]bool{"keys.json": true, "keys.lock": true, "hmac.key": true, "hmac.lock": true, "usage.lock": true, "usage.sqlite": true, "usage.sqlite-wal": true, "usage.sqlite-shm": true}
	for _, entry := range entries {
		if !allowed[entry.Name()] || !entry.Type().IsRegular() {
			return useridentity.ErrResetUnsupported
		}
	}
	if err := s.Initialize(ctx); err != nil {
		return err
	}
	if err := s.Recover(ctx, time.Now()); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return storage.ErrUnavailable
	}
	defer tx.Rollback()
	state := struct {
		Version     int    `json:"version"`
		OperationID string `json:"operation_id"`
		State       string `json:"state"`
	}{Version: 1, OperationID: events.New(events.Paused, time.Now()).ID, State: "intent"}
	var prior string
	err = tx.QueryRowContext(ctx, `SELECT value_json FROM runtime_state WHERE namespace='identity' AND key='reset'`).Scan(&prior)
	if err == nil {
		if json.Unmarshal([]byte(prior), &state) != nil || state.Version != 1 || !hexID.MatchString(state.OperationID) || state.State != "complete" {
			return storage.ErrInvalid
		}
		state.State = "intent"
	} else if !errors.Is(err, sql.ErrNoRows) {
		return storage.ErrUnavailable
	}
	intent, _ := json.Marshal(state)
	if _, err = tx.ExecContext(ctx, `INSERT INTO runtime_state VALUES('identity','reset',?,?) ON CONFLICT(namespace,key) DO UPDATE SET value_json=excluded.value_json,updated_at=excluded.updated_at`, string(intent), time.Now().UTC().UnixMicro()); err != nil {
		return storage.ErrUnavailable
	}
	for _, q := range []string{`DELETE FROM requests WHERE user_hash IS NOT NULL`, `DELETE FROM events WHERE user_hash IS NOT NULL`, `DELETE FROM usage_daily WHERE scope='user'`, `DELETE FROM budget_state WHERE scope='user'`} {
		if _, err = tx.ExecContext(ctx, q); err != nil {
			return storage.ErrUnavailable
		}
	}
	state.State = "complete"
	completed, _ := json.Marshal(state)
	if _, err = tx.ExecContext(ctx, `UPDATE runtime_state SET value_json=?,updated_at=? WHERE namespace='identity' AND key='reset'`, string(completed), time.Now().UTC().UnixMicro()); err != nil {
		return storage.ErrUnavailable
	}
	if err = tx.Commit(); err != nil {
		return storage.ErrUnavailable
	}
	return nil
}
