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

// HistoricalApps lists every app with persisted state, configured or not.
func (s *Store) HistoricalApps(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT app_name FROM requests
UNION SELECT app_name FROM usage_daily
UNION SELECT app_name FROM events WHERE app_name IS NOT NULL
UNION SELECT app_name FROM budget_state
UNION SELECT app_name FROM limit_reservations
UNION SELECT app_name FROM limit_rate_events
UNION SELECT json_extract(metadata_json,'$.AppName') FROM metering_journal
UNION SELECT key FROM runtime_state WHERE namespace=?`, namespacePauseApp)
	if err != nil {
		return nil, storage.ErrUnavailable
	}
	defer rows.Close()
	var apps []string
	for rows.Next() {
		var app sql.NullString
		if err = rows.Scan(&app); err != nil {
			return nil, storage.ErrUnavailable
		}
		if app.Valid && recordName.MatchString(app.String) {
			apps = append(apps, app.String)
		}
	}
	if rows.Err() != nil {
		return nil, storage.ErrUnavailable
	}
	return apps, nil
}

const (
	forgetMinBackoff = 50 * time.Millisecond
	forgetMaxBackoff = 500 * time.Millisecond
)

// forgetRetryWindow is how long a busy database is retried. Tests shorten it.
var forgetRetryWindow = 10 * time.Second

// ForgetSubjects deletes the history of identified users across apps in one
// transaction: all or nothing. A busy or locked database repeats the whole
// transaction for up to 10 seconds; any other failure is final. App totals,
// reservations and rate obligations stay, without the link to the users.
func (s *Store) ForgetSubjects(ctx context.Context, subjects []storage.SubjectRef) (storage.ForgetCounts, error) {
	for _, subject := range subjects {
		if !recordName.MatchString(subject.App) || !hexID.MatchString(subject.Hash) {
			return storage.ForgetCounts{}, storage.ErrInvalid
		}
	}
	deadline := time.Now().Add(forgetRetryWindow)
	backoff := forgetMinBackoff
	for {
		counts, err := s.forgetOnce(ctx, subjects)
		if err == nil || !isBusy(err) {
			return counts, mapForgetError(err)
		}
		if time.Now().Add(backoff).After(deadline) {
			return storage.ForgetCounts{}, storage.ErrUnavailable
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return storage.ForgetCounts{}, storage.ErrUnavailable
		case <-timer.C:
		}
		backoff = min(backoff*2, forgetMaxBackoff)
	}
}

func mapForgetError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, storage.ErrBusy), errors.Is(err, storage.ErrInvalid), errors.Is(err, storage.ErrFull), errors.Is(err, storage.ErrUnavailable):
		return err
	}
	return databaseError(err)
}

// rawBusy keeps a driver error intact so the retry loop can recognize it.
type rawBusy struct{ cause error }

func (e rawBusy) Error() string { return "database busy" }
func (e rawBusy) Unwrap() error { return e.cause }

func isBusy(err error) bool {
	var busy rawBusy
	return errors.As(err, &busy)
}

// wrap marks busy and locked driver errors for a retry and maps the rest.
func wrap(err error) error {
	if err == nil {
		return nil
	}
	if code, ok := sqliteCode(err); ok && (code == 5 || code == 6) {
		return rawBusy{err}
	}
	return databaseError(err)
}

func (s *Store) forgetOnce(ctx context.Context, subjects []storage.SubjectRef) (storage.ForgetCounts, error) {
	var counts storage.ForgetCounts
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return counts, wrap(err)
	}
	defer tx.Rollback()
	exec := func(query string, n *int64, args ...any) error {
		result, err := tx.ExecContext(ctx, query, args...)
		if err != nil {
			return wrap(err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return wrap(err)
		}
		*n += affected
		return nil
	}
	for _, subject := range subjects {
		var reserved int64
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM limit_reservations WHERE app_name=? AND subject=? AND anonymous=0 AND state='reserved'`, subject.App, subject.Hash).Scan(&reserved); err != nil {
			return counts, wrap(err)
		}
		if reserved > 0 {
			return counts, storage.ErrBusy
		}
		if err = forgetJournal(ctx, tx, subject, &counts.Journal); err != nil {
			return counts, err
		}
		if err = exec(`DELETE FROM events WHERE (app_name=? AND user_hash=? AND anonymous=0) OR request_id IN (SELECT request_id FROM requests WHERE app_name=? AND user_hash=? AND anonymous=0)`, &counts.Events, subject.App, subject.Hash, subject.App, subject.Hash); err != nil {
			return counts, err
		}
		if err = exec(`DELETE FROM requests WHERE app_name=? AND user_hash=? AND anonymous=0`, &counts.Requests, subject.App, subject.Hash); err != nil {
			return counts, err
		}
		if err = exec(`DELETE FROM usage_daily WHERE scope='user' AND app_name=? AND subject=? AND anonymous=0`, &counts.UsageDaily, subject.App, subject.Hash); err != nil {
			return counts, err
		}
		unlinked, err := unlinkLimitSubjectsCounted(ctx, tx, subject.App, subject.Hash, false)
		if err != nil {
			return counts, err
		}
		counts.ReservationItems += unlinked.items
		counts.BudgetState += unlinked.budgets
		counts.Unlinked += unlinked.unlinked
	}
	if err = tx.Commit(); err != nil {
		return storage.ForgetCounts{}, wrap(err)
	}
	if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM metering_journal`).Scan(&s.journalCount); err != nil {
		s.journalCount = 0
	}
	return counts, nil
}

// forgetJournal deletes journal residue already materialized into requests. An
// entry without its request row is usage still in transit: deleting it would
// lose consumption, keeping it would bring the user back on recovery.
func forgetJournal(ctx context.Context, tx *sql.Tx, subject storage.SubjectRef, count *int64) error {
	rows, err := tx.QueryContext(ctx, `SELECT j.request_id,j.attempt_index,j.state,EXISTS(SELECT 1 FROM requests r WHERE r.request_id=j.request_id AND r.attempt_index=j.attempt_index)
FROM metering_journal j WHERE json_extract(j.metadata_json,'$.AppName')=? AND json_extract(j.metadata_json,'$.UserHash')=? AND json_extract(j.metadata_json,'$.Anonymous')=0`, subject.App, subject.Hash)
	if err != nil {
		return wrap(err)
	}
	type key struct {
		requestID string
		index     int
	}
	var residue []key
	for rows.Next() {
		var k key
		var state string
		var materialized bool
		if err = rows.Scan(&k.requestID, &k.index, &state, &materialized); err != nil {
			rows.Close()
			return wrap(err)
		}
		if state != "completed" || !materialized {
			rows.Close()
			return storage.ErrBusy
		}
		residue = append(residue, k)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return wrap(err)
	}
	for _, k := range residue {
		if _, err = tx.ExecContext(ctx, `DELETE FROM metering_journal WHERE request_id=? AND attempt_index=?`, k.requestID, k.index); err != nil {
			return wrap(err)
		}
		*count++
	}
	return nil
}
