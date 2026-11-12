package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/JoaoVictorVM/comporta/internal/events"
	"github.com/JoaoVictorVM/comporta/internal/storage"
	"strings"
	"time"
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
	inserted, err := putEvent(ctx, tx, e, string(payload))
	if err != nil || !inserted {
		return err
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

// putEvent inserts an event or confirms an identical one, subject included. It
// reports whether a new row was written, which is what deserves a notification.
func putEvent(ctx context.Context, tx *sql.Tx, e events.Event, payload string) (bool, error) {
	var previousType, previousPayload, previousRequest, previousApp, previousHash string
	var previousTime int64
	var previousAnonymous sql.NullBool
	err := tx.QueryRowContext(ctx, `SELECT type,payload_json,coalesce(request_id,''),coalesce(app_name,''),coalesce(user_hash,''),anonymous,occurred_at FROM events WHERE event_id=?`, e.ID).
		Scan(&previousType, &previousPayload, &previousRequest, &previousApp, &previousHash, &previousAnonymous, &previousTime)
	if err == nil {
		sameSubject := previousHash == e.UserHash && previousAnonymous.Valid == (e.Anonymous != nil) && (e.Anonymous == nil || previousAnonymous.Bool == *e.Anonymous)
		if previousType == string(e.Type) && previousPayload == payload && previousRequest == e.RequestID && previousApp == e.App && previousTime == e.At.UnixMicro() && sameSubject {
			return false, nil
		}
		return false, storage.ErrConflict
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, databaseError(err)
	}
	var anonymous any
	if e.Anonymous != nil {
		anonymous = *e.Anonymous
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO events(event_id,type,occurred_at,request_id,app_name,user_hash,anonymous,payload_json) VALUES(?,?,?,nullif(?,''),nullif(?,''),nullif(?,''),?,?)`,
		e.ID, string(e.Type), e.At.UnixMicro(), e.RequestID, e.App, e.UserHash, anonymous, payload)
	if err != nil {
		return false, databaseError(err)
	}
	return true, nil
}

func (s *Store) ReadEvents(ctx context.Context, f events.Filter) (events.Page, error) {
	var page events.Page
	limit := f.Limit
	if limit == 0 {
		limit = 100
	}
	if limit < 1 || limit > 1000 {
		return page, storage.ErrInvalid
	}
	from, until := int64(-1<<63), int64(1<<63-1)
	if !f.From.IsZero() {
		from = f.From.UnixMicro()
	}
	if !f.Until.IsZero() {
		until = f.Until.UnixMicro()
	}
	afterTime, afterID := int64(-1<<63), ""
	if f.After != nil {
		afterTime = f.After.At.UnixMicro()
		afterID = f.After.ID
	}
	query := `SELECT event_id,type,occurred_at,coalesce(request_id,''),coalesce(app_name,''),coalesce(user_hash,''),anonymous,payload_json FROM events WHERE (?='' OR app_name=?) AND occurred_at>=? AND occurred_at<? AND (occurred_at,event_id)>(?,?)`
	args := []any{f.App, f.App, from, until, afterTime, afterID}
	if len(f.Types) > 0 {
		query += ` AND type IN (` + strings.TrimSuffix(strings.Repeat("?,", len(f.Types)), ",") + `)`
		for _, kind := range f.Types {
			if !events.ValidType(kind) {
				return page, storage.ErrInvalid
			}
			args = append(args, string(kind))
		}
	}
	query += ` ORDER BY occurred_at,event_id LIMIT ?`
	args = append(args, limit+1)
	db := s.readDB
	if db == nil {
		db = s.db
	}
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return page, storage.ErrUnavailable
	}
	defer rows.Close()
	for rows.Next() {
		var e events.Event
		var micros int64
		var payload, hash string
		var anonymous sql.NullBool
		if err = rows.Scan(&e.ID, &e.Type, &micros, &e.RequestID, &e.App, &hash, &anonymous, &payload); err != nil {
			return page, storage.ErrUnavailable
		}
		// Only loop cuts carry a subject in the event contract.
		if e.Type == events.LoopCut && hash != "" && anonymous.Valid {
			e.UserHash = hash
			flag := anonymous.Bool
			e.Anonymous = &flag
		}
		e.At = time.UnixMicro(micros).UTC()
		if err = json.Unmarshal([]byte(payload), &e.Payload); err != nil {
			return page, storage.ErrInvalid
		}
		if err = e.Validate(); err != nil {
			return page, storage.ErrInvalid
		}
		if len(page.Events) == limit {
			last := page.Events[len(page.Events)-1]
			page.Next = &events.Cursor{At: last.At, ID: last.ID}
			break
		}
		page.Events = append(page.Events, e)
	}
	return page, rows.Err()
}
