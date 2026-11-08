package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/JoaoVictorVM/comporta/internal/events"
	"github.com/JoaoVictorVM/comporta/internal/storage"
	"github.com/JoaoVictorVM/comporta/internal/useridentity"
	"io"
	"os"
	"strings"
	"time"
)

type resetState struct {
	Version     int    `json:"version"`
	OperationID string `json:"operation_id"`
	State       string `json:"state"`
}

func decodeReset(data string, state *resetState) error {
	d := json.NewDecoder(strings.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(state) != nil || d.Decode(new(any)) != io.EOF || state.Version != 1 || !hexID.MatchString(state.OperationID) || state.State != "complete" {
		return storage.ErrInvalid
	}
	return nil
}

func (s *Store) inspectRuntime(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT namespace,key,value_json FROM runtime_state`)
	if err != nil {
		return storage.ErrUnavailable
	}
	defer rows.Close()
	for rows.Next() {
		var namespace, key, data string
		if err = rows.Scan(&namespace, &key, &data); err != nil {
			return storage.ErrUnavailable
		}
		if namespace != "identity" || key != "reset" {
			return storage.ErrInvalid
		}
		var state resetState
		if err = decodeReset(data, &state); err != nil {
			return err
		}
	}
	if rows.Err() != nil {
		return storage.ErrUnavailable
	}
	return nil
}

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
		found := false
		for _, e := range entries {
			if e.Name() == "usage.sqlite" {
				found = true
			}
		}
		if found {
			reader, err := openReader(ctx, s.dir, true)
			if err != nil {
				return useridentity.HistoryPresent, err
			}
			s.db = reader.db
		} else {
			for _, e := range entries {
				if e.Name() == "usage.sqlite-wal" || e.Name() == "usage.sqlite-shm" {
					return useridentity.HistoryPresent, storage.ErrUnavailable
				}
			}
			return useridentity.HistoryEmpty, nil
		}
	}
	version, err := checkSchema(ctx, s.db)
	if err != nil {
		return useridentity.HistoryPresent, err
	}
	if err := s.inspectRuntime(ctx); err != nil {
		return useridentity.HistoryPresent, err
	}
	queries := []string{`SELECT count(*) FROM requests WHERE user_hash IS NOT NULL`, `SELECT count(*) FROM usage_daily WHERE scope='user'`, `SELECT count(*) FROM metering_journal`, `SELECT count(*) FROM budget_state WHERE scope='user'`, `SELECT count(*) FROM events WHERE user_hash IS NOT NULL`}
	// A database of the previous release has no limit tables yet; it is inspected
	// as it is, before any migration.
	if version >= 2 {
		queries = append(queries, userLimitQueries...)
	}
	for _, query := range queries {
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
	if err := s.inspectRuntime(ctx); err != nil {
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
	state := resetState{Version: 1, OperationID: events.New(events.Paused, time.Now()).ID, State: "intent"}
	var prior string
	err = tx.QueryRowContext(ctx, `SELECT value_json FROM runtime_state WHERE namespace='identity' AND key='reset'`).Scan(&prior)
	if err == nil {
		if decodeReset(prior, &state) != nil {
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
	if err = unlinkLimitSubjects(ctx, tx, "", "", false); err != nil {
		return err
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
