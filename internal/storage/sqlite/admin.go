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
