package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/events"
	"github.com/JoaoVictorVM/comporta/internal/storage"
)

// Namespaces of runtime_state owned by the admin operations.
const (
	namespacePauseGlobal = "pause_global"
	namespacePauseApp    = "pause_app"
	namespaceAdmin       = "admin"
	keyPauseGlobal       = "state"
	keyTokenInitialized  = "token_initialized"
)

type pauseValue struct {
	Version int   `json:"version"`
	Paused  *bool `json:"paused"`
}

type markerValue struct {
	Version     int   `json:"version"`
	Initialized *bool `json:"initialized"`
}

func decodeRuntime(data string, target any) error {
	decoder := json.NewDecoder(bytes.NewBufferString(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(target) != nil || decoder.Decode(new(any)) != io.EOF {
		return storage.ErrInvalid
	}
	return nil
}

func decodePause(data string) (bool, error) {
	var value pauseValue
	if err := decodeRuntime(data, &value); err != nil || value.Version != 1 || value.Paused == nil {
		return false, storage.ErrInvalid
	}
	return *value.Paused, nil
}

// validateRuntimeEntry accepts only the admin namespaces this version writes.
// An unknown namespace or a future version stays an error, never ignored.
func validateRuntimeEntry(namespace, key, data string) error {
	switch namespace {
	case namespacePauseGlobal:
		if key != keyPauseGlobal {
			return storage.ErrInvalid
		}
		_, err := decodePause(data)
		return err
	case namespacePauseApp:
		if !recordName.MatchString(key) {
			return storage.ErrInvalid
		}
		_, err := decodePause(data)
		return err
	case namespaceAdmin:
		var value markerValue
		if key != keyTokenInitialized || decodeRuntime(data, &value) != nil || value.Version != 1 || value.Initialized == nil || !*value.Initialized {
			return storage.ErrInvalid
		}
		return nil
	}
	return storage.ErrInvalid
}

// AdminInitialized reports whether an admin token was created for this data
// directory before.
func (s *Store) AdminInitialized(ctx context.Context) (bool, error) {
	var data string
	err := s.db.QueryRowContext(ctx, `SELECT value_json FROM runtime_state WHERE namespace=? AND key=?`, namespaceAdmin, keyTokenInitialized).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, storage.ErrUnavailable
	}
	if err := validateRuntimeEntry(namespaceAdmin, keyTokenInitialized, data); err != nil {
		return false, err
	}
	return true, nil
}

// MarkAdminInitialized records the marker. It is a fact, not a credential.
func (s *Store) MarkAdminInitialized(ctx context.Context) error {
	initialized := true
	data, _ := json.Marshal(markerValue{Version: 1, Initialized: &initialized})
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `INSERT INTO runtime_state VALUES(?,?,?,?) ON CONFLICT(namespace,key) DO UPDATE SET value_json=excluded.value_json,updated_at=excluded.updated_at`,
		namespaceAdmin, keyTokenInitialized, string(data), time.Now().UTC().UnixMicro())
	if err != nil {
		return databaseError(err)
	}
	return nil
}

type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func loadPause(ctx context.Context, db queryer) (storage.PauseState, error) {
	var state storage.PauseState
	rows, err := db.QueryContext(ctx, `SELECT namespace,key,value_json,updated_at FROM runtime_state WHERE namespace IN(?,?) ORDER BY namespace,key`, namespacePauseGlobal, namespacePauseApp)
	if err != nil {
		return state, storage.ErrUnavailable
	}
	defer rows.Close()
	for rows.Next() {
		var namespace, key, data string
		var micros int64
		if err = rows.Scan(&namespace, &key, &data, &micros); err != nil {
			return state, storage.ErrUnavailable
		}
		if err = validateRuntimeEntry(namespace, key, data); err != nil {
			return state, err
		}
		paused, _ := decodePause(data)
		at := time.UnixMicro(micros).UTC()
		if namespace == namespacePauseGlobal {
			state.Global, state.GlobalUpdatedAt = paused, at
			continue
		}
		if paused {
			state.Apps = append(state.Apps, storage.AppPause{App: key, Paused: true, UpdatedAt: at})
		}
	}
	if rows.Err() != nil {
		return state, storage.ErrUnavailable
	}
	return state, nil
}

// LoadPause reads every pause override.
func (s *Store) LoadPause(ctx context.Context) (storage.PauseState, error) {
	return loadPause(ctx, s.db)
}

// SavePause applies one operator action and writes its pause event in the same
// transaction. The event is announced on the bus only after the commit.
func (s *Store) SavePause(ctx context.Context, change storage.PauseChange) (storage.PauseState, bool, error) {
	if change.At.IsZero() || (change.All == (change.App != "")) || (!change.All && !recordName.MatchString(change.App)) {
		return storage.PauseState{}, false, storage.ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return storage.PauseState{}, false, databaseError(err)
	}
	defer tx.Rollback()
	before, err := loadPause(ctx, tx)
	if err != nil {
		return storage.PauseState{}, false, err
	}
	at := change.At.UTC()
	value := func(paused bool) string {
		data, _ := json.Marshal(pauseValue{Version: 1, Paused: &paused})
		return string(data)
	}
	upsert := `INSERT INTO runtime_state VALUES(?,?,?,?) ON CONFLICT(namespace,key) DO UPDATE SET value_json=excluded.value_json,updated_at=excluded.updated_at`
	changed := false
	switch {
	case change.All && change.Paused:
		changed = !before.Global
		if changed {
			_, err = tx.ExecContext(ctx, upsert, namespacePauseGlobal, keyPauseGlobal, value(true), at.UnixMicro())
		}
	case change.All:
		changed = before.Global || len(before.Apps) > 0
		if changed {
			if _, err = tx.ExecContext(ctx, upsert, namespacePauseGlobal, keyPauseGlobal, value(false), at.UnixMicro()); err == nil {
				_, err = tx.ExecContext(ctx, `DELETE FROM runtime_state WHERE namespace=?`, namespacePauseApp)
			}
		}
	case change.Paused:
		changed = !appPaused(before, change.App)
		if changed {
			_, err = tx.ExecContext(ctx, upsert, namespacePauseApp, change.App, value(true), at.UnixMicro())
		}
	default:
		changed = appPaused(before, change.App)
		if changed {
			_, err = tx.ExecContext(ctx, `DELETE FROM runtime_state WHERE namespace=? AND key=?`, namespacePauseApp, change.App)
		}
	}
	if err != nil {
		return storage.PauseState{}, false, databaseError(err)
	}
	if !changed {
		return before, false, nil
	}
	event := events.New(events.Paused, at)
	paused := change.Paused
	event.Payload.Paused = &paused
	event.Payload.Scope = "app"
	event.App = change.App
	if change.All {
		event.Payload.Scope = "global"
	}
	if err = event.Validate(); err != nil {
		return storage.PauseState{}, false, storage.ErrInvalid
	}
	payload, _ := json.Marshal(event.Payload)
	if _, err = tx.ExecContext(ctx, `INSERT INTO events(event_id,type,occurred_at,app_name,payload_json) VALUES(?,?,?,nullif(?,''),?)`, event.ID, string(event.Type), event.At.UnixMicro(), event.App, string(payload)); err != nil {
		return storage.PauseState{}, false, databaseError(err)
	}
	after, err := loadPause(ctx, tx)
	if err != nil {
		return storage.PauseState{}, false, err
	}
	if err = tx.Commit(); err != nil {
		return storage.PauseState{}, false, databaseError(err)
	}
	s.Bus.Notify(event)
	return after, true, nil
}

func appPaused(state storage.PauseState, app string) bool {
	for _, entry := range state.Apps {
		if entry.App == app {
			return entry.Paused
		}
	}
	return false
}
