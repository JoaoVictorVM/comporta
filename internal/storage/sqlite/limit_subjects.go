package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"

	"github.com/JoaoVictorVM/comporta/internal/storage"
)

// userLimitQueries find any user-linked row in the limit tables.
var userLimitQueries = []string{
	`SELECT count(*) FROM limit_reservations WHERE subject<>''`,
	`SELECT count(*) FROM limit_reservation_items WHERE scope='user'`,
	`SELECT count(*) FROM limit_rate_events WHERE subject<>''`,
	`SELECT count(*) FROM limit_ledger WHERE payload_json LIKE '%"scope":"user"%'`,
	`SELECT count(*) FROM limit_checkpoints WHERE metadata_json LIKE '%"scope":"user"%'`,
}

// unlinkLimitSubjects removes the user side of limit state while keeping every
// app obligation: reservations, rate events and ledger deltas of the app stay,
// only the link to the user goes. An empty app unlinks every user.
func unlinkLimitSubjects(ctx context.Context, tx *sql.Tx, app, subject string, anonymous bool) error {
	all := app == ""
	match := func(column string) (string, []any) {
		if all {
			return column + `<>''`, nil
		}
		return `app_name=? AND ` + column + `=? AND anonymous=?`, []any{app, subject, anonymous}
	}
	reservations, args := match("subject")
	itemFilter := `scope='user'`
	var itemArgs []any
	if !all {
		itemFilter = `scope='user' AND subject=? AND anonymous=? AND EXISTS(SELECT 1 FROM limit_reservations r WHERE r.request_id=limit_reservation_items.request_id AND r.attempt_index=limit_reservation_items.attempt_index AND r.app_name=?)`
		itemArgs = []any{subject, anonymous, app}
	}
	steps := []struct {
		query string
		args  []any
	}{
		{`DELETE FROM limit_reservation_items WHERE ` + itemFilter, itemArgs},
		{`UPDATE limit_reservations SET subject='',anonymous=0 WHERE ` + reservations, args},
		{`UPDATE limit_rate_events SET subject='',anonymous=0 WHERE ` + reservations, args},
	}
	if !all {
		steps = append(steps, struct {
			query string
			args  []any
		}{`DELETE FROM budget_state WHERE scope='user' AND app_name=? AND subject=? AND anonymous=?`, []any{app, subject, anonymous}})
	}
	for _, step := range steps {
		if _, err := tx.ExecContext(ctx, step.query, step.args...); err != nil {
			return databaseError(err)
		}
	}
	keep := func(entryApp, scope, entrySubject string, entryAnonymous bool) bool {
		if scope != "user" {
			return true
		}
		return !all && (entryApp != app || entrySubject != subject || entryAnonymous != anonymous)
	}
	rows, err := tx.QueryContext(ctx, `SELECT seq,payload_json FROM limit_ledger WHERE payload_json LIKE '%"scope":"user"%'`)
	if err != nil {
		return databaseError(err)
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
			return databaseError(err)
		}
		var payload ledgerPayload
		if err = decodeStrict(data, &payload); err != nil {
			rows.Close()
			return err
		}
		kept := payload.Deltas[:0]
		for _, d := range payload.Deltas {
			if keep(payload.AppName, d.Scope, d.Subject, d.Anonymous) {
				kept = append(kept, d)
			}
		}
		payload.Deltas = kept
		updated, _ := json.Marshal(payload)
		rewrites = append(rewrites, rewrite{seq, string(updated)})
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return databaseError(err)
	}
	for _, r := range rewrites {
		if _, err = tx.ExecContext(ctx, `UPDATE limit_ledger SET payload_json=? WHERE seq=?`, r.data, r.seq); err != nil {
			return databaseError(err)
		}
	}
	var metadata string
	err = tx.QueryRowContext(ctx, `SELECT metadata_json FROM limit_checkpoints WHERE id=1`).Scan(&metadata)
	if err == sql.ErrNoRows || (err == nil && !strings.Contains(metadata, `"scope":"user"`)) {
		return nil
	}
	if err != nil {
		return databaseError(err)
	}
	var meta storage.CheckpointMetadata
	if err = decodeStrict(metadata, &meta); err != nil {
		return err
	}
	kept := meta.Unknown[:0]
	for _, ref := range meta.Unknown {
		if keep(ref.AppName, ref.Scope, ref.Subject, ref.Anonymous) {
			kept = append(kept, ref)
		}
	}
	meta.Unknown = kept
	updated, _ := json.Marshal(meta)
	if _, err = tx.ExecContext(ctx, `UPDATE limit_checkpoints SET metadata_json=? WHERE id=1`, string(updated)); err != nil {
		return databaseError(err)
	}
	return nil
}

// ForgetLimitSubject removes one user of one app from the persisted limit
// state, keeping the app's spending and obligations. It is the persistence
// contract for per-user deletion; the coordinator must not be running, since
// its memory still holds that user until the next recovery.
func (s *Store) ForgetLimitSubject(ctx context.Context, app, subject string, anonymous bool) error {
	if !recordName.MatchString(app) || !hexID.MatchString(subject) {
		return storage.ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return databaseError(err)
	}
	defer tx.Rollback()
	if err = unlinkLimitSubjects(ctx, tx, app, subject, anonymous); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return databaseError(err)
	}
	return nil
}
