package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/events"
	"github.com/JoaoVictorVM/comporta/internal/storage"
)

func configMicrousd(v int64) config.Microusd { return config.Microusd(v) }

const (
	namespacePauseGlobal = "pause_global"
	namespacePauseApp    = "pause_app"
	namespaceZone        = "limit_zone"
	keyPauseGlobal       = "state"
)

type pauseValue struct {
	Version int   `json:"version"`
	Paused  *bool `json:"paused"`
}

func decodePause(data string) (bool, error) {
	var value pauseValue
	if err := storage.DecodeStrict(data, &value); err != nil || value.Version != 1 || value.Paused == nil {
		return false, storage.ErrInvalid
	}
	return *value.Paused, nil
}

type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func loadPause(ctx context.Context, db queryer) (storage.PauseState, error) {
	var state storage.PauseState
	rows, err := db.QueryContext(ctx, `SELECT namespace,key,value_json::text,updated_at FROM runtime_state WHERE namespace IN($1,$2) ORDER BY namespace,key`, namespacePauseGlobal, namespacePauseApp)
	if err != nil {
		return state, storage.ErrUnavailable
	}
	defer rows.Close()
	for rows.Next() {
		var namespace, key, data string
		var at int64
		if err = rows.Scan(&namespace, &key, &data, &at); err != nil {
			return state, storage.ErrUnavailable
		}
		paused, err := decodePause(data)
		if err != nil || (namespace == namespacePauseGlobal && key != keyPauseGlobal) || (namespace == namespacePauseApp && !storage.RecordName.MatchString(key)) {
			return state, storage.ErrInvalid
		}
		when := time.UnixMicro(at).UTC()
		if namespace == namespacePauseGlobal {
			state.Global, state.GlobalUpdatedAt = paused, when
			continue
		}
		if paused {
			state.Apps = append(state.Apps, storage.AppPause{App: key, Paused: true, UpdatedAt: when})
		}
	}
	if rows.Err() != nil {
		return state, storage.ErrUnavailable
	}
	return state, nil
}

// LoadPause reads the durable pause overrides, the source every admission
// checks in its gate.
func (s *Store) LoadPause(ctx context.Context) (storage.PauseState, error) {
	if sc := scopeFrom(ctx); sc != nil {
		return loadPause(ctx, sc.Tx)
	}
	return loadPause(ctx, s.db)
}

// PauseIn reads the pause state inside an admission transaction.
func PauseIn(ctx context.Context, tx *sql.Tx) (storage.PauseState, error) { return loadPause(ctx, tx) }

// SavePause applies one operator action and its event in one transaction,
// and bumps the control revision that replicas compare to refresh their
// projection. The coordinator calls it under the exclusive gate.
func (s *Store) SavePause(ctx context.Context, change storage.PauseChange) (storage.PauseState, bool, error) {
	if change.At.IsZero() || (change.All == (change.App != "")) || (!change.All && !storage.RecordName.MatchString(change.App)) {
		return storage.PauseState{}, false, storage.ErrInvalid
	}
	var after storage.PauseState
	changed := false
	err := s.inTx(ctx, func(tx *sql.Tx) (func(), error) {
		before, err := loadPause(ctx, tx)
		if err != nil {
			return nil, err
		}
		at := change.At.UTC()
		value := func(paused bool) string {
			data, _ := json.Marshal(pauseValue{Version: 1, Paused: &paused})
			return string(data)
		}
		const upsert = `INSERT INTO runtime_state VALUES($1,$2,$3,$4) ON CONFLICT(namespace,key) DO UPDATE SET value_json=excluded.value_json,updated_at=excluded.updated_at`
		switch {
		case change.All && change.Paused:
			if changed = !before.Global; changed {
				_, err = tx.ExecContext(ctx, upsert, namespacePauseGlobal, keyPauseGlobal, value(true), micros(at))
			}
		case change.All:
			if changed = before.Global || len(before.Apps) > 0; changed {
				if _, err = tx.ExecContext(ctx, upsert, namespacePauseGlobal, keyPauseGlobal, value(false), micros(at)); err == nil {
					_, err = tx.ExecContext(ctx, `DELETE FROM runtime_state WHERE namespace=$1`, namespacePauseApp)
				}
			}
		case change.Paused:
			if changed = !appPaused(before, change.App); changed {
				_, err = tx.ExecContext(ctx, upsert, namespacePauseApp, change.App, value(true), micros(at))
			}
		default:
			if changed = appPaused(before, change.App); changed {
				_, err = tx.ExecContext(ctx, `DELETE FROM runtime_state WHERE namespace=$1 AND key=$2`, namespacePauseApp, change.App)
			}
		}
		if err != nil {
			return nil, databaseError(err)
		}
		if !changed {
			after = before
			return nil, nil
		}
		if _, err = tx.ExecContext(ctx, `UPDATE cluster_state SET control_revision=control_revision+1,updated_at=$1`, nowMicros()); err != nil {
			return nil, databaseError(err)
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
			return nil, storage.ErrInvalid
		}
		payload, _ := json.Marshal(event.Payload)
		if _, err = putEvent(ctx, tx, event, string(payload)); err != nil {
			return nil, err
		}
		if after, err = loadPause(ctx, tx); err != nil {
			return nil, err
		}
		return func() { s.Bus.Notify(event) }, nil
	})
	if err != nil {
		return storage.PauseState{}, false, err
	}
	return after, changed, nil
}

func appPaused(state storage.PauseState, app string) bool {
	for _, entry := range state.Apps {
		if entry.App == app {
			return entry.Paused
		}
	}
	return false
}

// AdminInitialized reports whether a distributed admin credential exists.
func (s *Store) AdminInitialized(ctx context.Context) (bool, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM admin_credentials`).Scan(&n); err != nil {
		return false, storage.ErrUnavailable
	}
	return n > 0, nil
}

// MarkAdminInitialized is implied by the credential row itself.
func (s *Store) MarkAdminInitialized(context.Context) error { return nil }

// HistoricalApps lists every app with persisted state, configured or not.
func (s *Store) HistoricalApps(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT app_name FROM requests
UNION SELECT app_name FROM usage_daily
UNION SELECT app_name FROM events WHERE app_name IS NOT NULL
UNION SELECT app_name FROM budget_state
UNION SELECT app_name FROM limit_reservations
UNION SELECT app_name FROM limit_rate_events
UNION SELECT metadata_json->>'AppName' FROM metering_journal
UNION SELECT app_name FROM cache_epochs
UNION SELECT key FROM runtime_state WHERE namespace=$1`, namespacePauseApp)
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
		if app.Valid && storage.RecordName.MatchString(app.String) {
			apps = append(apps, app.String)
		}
	}
	if rows.Err() != nil {
		return nil, storage.ErrUnavailable
	}
	return apps, nil
}

// ForgetSubjects deletes the history of identified users in one transaction,
// keeping app totals and obligations without the link to the users. Pending
// obligations of the users refuse with ErrBusy and change nothing.
func (s *Store) ForgetSubjects(ctx context.Context, subjects []storage.SubjectRef) (storage.ForgetCounts, error) {
	for _, subject := range subjects {
		if !storage.RecordName.MatchString(subject.App) || !storage.HexID.MatchString(subject.Hash) {
			return storage.ForgetCounts{}, storage.ErrInvalid
		}
	}
	var counts storage.ForgetCounts
	err := s.inTx(ctx, func(tx *sql.Tx) (func(), error) {
		counts = storage.ForgetCounts{}
		return nil, forgetTx(ctx, tx, subjects, &counts)
	})
	if err != nil {
		return storage.ForgetCounts{}, err
	}
	return counts, nil
}

func forgetTx(ctx context.Context, tx *sql.Tx, subjects []storage.SubjectRef, counts *storage.ForgetCounts) error {
	exec := func(query string, n *int64, args ...any) error {
		result, err := tx.ExecContext(ctx, query, args...)
		if err != nil {
			return databaseError(err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return databaseError(err)
		}
		*n += affected
		return nil
	}
	for _, subject := range subjects {
		var reserved int64
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM limit_reservations WHERE app_name=$1 AND subject=$2 AND NOT anonymous AND state='reserved'`, subject.App, subject.Hash).Scan(&reserved); err != nil {
			return databaseError(err)
		}
		if reserved > 0 {
			return storage.BusyError{Reason: "reservation still reserved in storage"}
		}
		if err := forgetJournal(ctx, tx, subject, &counts.Journal); err != nil {
			return err
		}
		var active int64
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM request_operations WHERE app_name=$1 AND subject=$2 AND NOT anonymous AND state='active'`, subject.App, subject.Hash).Scan(&active); err != nil {
			return databaseError(err)
		}
		if active > 0 {
			return storage.BusyError{Reason: "fallback session still active in storage"}
		}
		var operations int64
		steps := []struct {
			query string
			n     *int64
			args  []any
		}{
			{`DELETE FROM request_operations WHERE app_name=$1 AND subject=$2 AND NOT anonymous`, &operations, []any{subject.App, subject.Hash}},
			{`DELETE FROM events WHERE (app_name=$1 AND user_hash=$2 AND anonymous=false) OR request_id IN (SELECT request_id FROM requests WHERE app_name=$1 AND user_hash=$2 AND anonymous=false)`, &counts.Events, []any{subject.App, subject.Hash}},
			{`DELETE FROM requests WHERE app_name=$1 AND user_hash=$2 AND anonymous=false`, &counts.Requests, []any{subject.App, subject.Hash}},
			{`DELETE FROM usage_daily WHERE scope='user' AND app_name=$1 AND subject=$2 AND NOT anonymous`, &counts.UsageDaily, []any{subject.App, subject.Hash}},
		}
		for _, step := range steps {
			if err := exec(step.query, step.n, step.args...); err != nil {
				return err
			}
		}
		unlinked, err := unlinkLimitSubjects(ctx, tx, subject.App, subject.Hash, false)
		if err != nil {
			return err
		}
		counts.ReservationItems += unlinked.items
		counts.BudgetState += unlinked.budgets
		counts.Unlinked += unlinked.unlinked
		if err := scrubOutbox(ctx, tx, subject); err != nil {
			return err
		}
	}
	return nil
}

func forgetJournal(ctx context.Context, tx *sql.Tx, subject storage.SubjectRef, count *int64) error {
	rows, err := tx.QueryContext(ctx, `SELECT j.request_id,j.attempt_index,j.state,EXISTS(SELECT 1 FROM requests r WHERE r.request_id=j.request_id AND r.attempt_index=j.attempt_index)
FROM metering_journal j WHERE j.metadata_json->>'AppName'=$1 AND j.metadata_json->>'UserHash'=$2 AND (j.metadata_json->>'Anonymous')::boolean=false`, subject.App, subject.Hash)
	if err != nil {
		return databaseError(err)
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
			return databaseError(err)
		}
		if state != "completed" || !materialized {
			rows.Close()
			return storage.BusyError{Reason: "usage journal entry not materialized"}
		}
		residue = append(residue, k)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return databaseError(err)
	}
	for _, k := range residue {
		if _, err = tx.ExecContext(ctx, `DELETE FROM metering_journal WHERE request_id=$1 AND attempt_index=$2`, k.requestID, k.index); err != nil {
			return databaseError(err)
		}
		*count++
	}
	return nil
}

// scrubOutbox removes the user from delivered and pending outbox payloads, so a
// replay can never write the link back into the shared projection.
func scrubOutbox(ctx context.Context, tx *sql.Tx, subject storage.SubjectRef) error {
	_, err := tx.ExecContext(ctx, `UPDATE coordination_outbox SET payload=jsonb_set(payload,'{subject}','""') WHERE app_name=$1 AND payload->>'subject'=$2`, subject.App, subject.Hash)
	if err != nil {
		return databaseError(err)
	}
	return nil
}

type unlinkCounts struct{ items, budgets, unlinked int64 }

// unlinkLimitSubjects removes the user side of limit state while keeping every
// app obligation.
func unlinkLimitSubjects(ctx context.Context, tx *sql.Tx, app, subject string, anonymous bool) (unlinkCounts, error) {
	var counts unlinkCounts
	steps := []struct {
		query string
		args  []any
		count *int64
	}{
		{`DELETE FROM limit_reservation_items WHERE scope='user' AND subject=$1 AND anonymous=$2 AND EXISTS(SELECT 1 FROM limit_reservations r WHERE r.request_id=limit_reservation_items.request_id AND r.attempt_index=limit_reservation_items.attempt_index AND r.app_name=$3)`, []any{subject, anonymous, app}, &counts.items},
		{`UPDATE limit_reservations SET subject='',anonymous=false WHERE app_name=$1 AND subject=$2 AND anonymous=$3`, []any{app, subject, anonymous}, &counts.unlinked},
		{`UPDATE limit_rate_events SET subject='',anonymous=false WHERE app_name=$1 AND subject=$2 AND anonymous=$3`, []any{app, subject, anonymous}, &counts.unlinked},
		{`UPDATE limit_attempt_rate_events SET subject='',anonymous=false WHERE app_name=$1 AND subject=$2 AND anonymous=$3`, []any{app, subject, anonymous}, &counts.unlinked},
		{`UPDATE request_operations SET subject='',anonymous=false WHERE app_name=$1 AND subject=$2 AND anonymous=$3`, []any{app, subject, anonymous}, &counts.unlinked},
		{`DELETE FROM budget_state WHERE scope='user' AND app_name=$1 AND subject=$2 AND anonymous=$3`, []any{app, subject, anonymous}, &counts.budgets},
	}
	for _, step := range steps {
		result, err := tx.ExecContext(ctx, step.query, step.args...)
		if err != nil {
			return counts, databaseError(err)
		}
		n, err := result.RowsAffected()
		if err != nil {
			return counts, databaseError(err)
		}
		*step.count += n
	}
	rows, err := tx.QueryContext(ctx, `SELECT seq,payload_json::text FROM limit_ledger WHERE payload_json->>'app_name'=$1 AND payload_json::text LIKE '%"scope": "user"%'`, app)
	if err != nil {
		return counts, databaseError(err)
	}
	type rewrite struct {
		seq  int64
		data string
	}
	var rewrites []rewrite
	for rows.Next() {
		var seq int64
		var data string
		if err = rows.Scan(&seq, &data); err != nil {
			rows.Close()
			return counts, databaseError(err)
		}
		var payload storage.LedgerPayload
		if err = storage.DecodeStrict(data, &payload); err != nil {
			rows.Close()
			return counts, err
		}
		kept := payload.Deltas[:0]
		for _, d := range payload.Deltas {
			if d.Scope != "user" || d.Subject != subject || d.Anonymous != anonymous {
				kept = append(kept, d)
			}
		}
		if len(kept) == len(payload.Deltas) {
			continue
		}
		payload.Deltas = kept
		updated, _ := json.Marshal(payload)
		rewrites = append(rewrites, rewrite{seq, string(updated)})
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return counts, databaseError(err)
	}
	for _, r := range rewrites {
		if _, err = tx.ExecContext(ctx, `UPDATE limit_ledger SET payload_json=$1 WHERE seq=$2`, r.data, r.seq); err != nil {
			return counts, databaseError(err)
		}
		counts.unlinked++
	}
	var metadata string
	err = tx.QueryRowContext(ctx, `SELECT metadata_json::text FROM limit_checkpoints WHERE id=1`).Scan(&metadata)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !strings.Contains(metadata, subject)) {
		return counts, nil
	}
	if err != nil {
		return counts, databaseError(err)
	}
	var meta storage.CheckpointMetadata
	if err = storage.DecodeStrict(metadata, &meta); err != nil {
		return counts, err
	}
	kept := meta.Unknown[:0]
	for _, ref := range meta.Unknown {
		if ref.Scope != "user" || ref.AppName != app || ref.Subject != subject || ref.Anonymous != anonymous {
			kept = append(kept, ref)
		}
	}
	if len(kept) == len(meta.Unknown) {
		return counts, nil
	}
	meta.Unknown = kept
	updated, _ := json.Marshal(meta)
	if _, err = tx.ExecContext(ctx, `UPDATE limit_checkpoints SET metadata_json=$1 WHERE id=1`, string(updated)); err != nil {
		return counts, databaseError(err)
	}
	counts.unlinked++
	return counts, nil
}

// LoadZones reads the durable timezone state of every app.
func LoadZones(ctx context.Context, q queryer) (map[string]storage.ZoneState, error) {
	rows, err := q.QueryContext(ctx, `SELECT key,value_json::text FROM runtime_state WHERE namespace=$1`, namespaceZone)
	if err != nil {
		return nil, storage.ErrUnavailable
	}
	defer rows.Close()
	out := map[string]storage.ZoneState{}
	for rows.Next() {
		var app, data string
		if err = rows.Scan(&app, &data); err != nil {
			return nil, storage.ErrUnavailable
		}
		var z storage.ZoneState
		if err = storage.DecodeStrict(data, &z); err != nil || !storage.RecordName.MatchString(app) {
			return nil, storage.ErrInvalid
		}
		out[app] = z
	}
	return out, rows.Err()
}

// SaveZone persists the timezone state of an app in the admission transaction
// that changed it.
func SaveZone(ctx context.Context, tx *sql.Tx, app string, z storage.ZoneState) error {
	data, _ := json.Marshal(z)
	if _, err := tx.ExecContext(ctx, `INSERT INTO runtime_state VALUES($1,$2,$3,$4) ON CONFLICT(namespace,key) DO UPDATE SET value_json=excluded.value_json,updated_at=excluded.updated_at`,
		namespaceZone, app, string(data), nowMicros()); err != nil {
		return databaseError(err)
	}
	return nil
}
