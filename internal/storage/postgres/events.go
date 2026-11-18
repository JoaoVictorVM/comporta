package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/events"
	"github.com/JoaoVictorVM/comporta/internal/storage"
)

// Publish persists an event idempotently. Every replica may publish the same
// cluster event; its identifier makes the second write a confirmation.
func (s *Store) Publish(ctx context.Context, e events.Event) error {
	if err := e.Validate(); err != nil {
		return err
	}
	payload, err := json.Marshal(e.Payload)
	if err != nil {
		return storage.ErrInvalid
	}
	return s.inTx(ctx, func(tx *sql.Tx) (func(), error) {
		inserted, err := putEvent(ctx, tx, e, string(payload))
		if err != nil || !inserted {
			return nil, err
		}
		if e.Type == events.ConfigChanged || e.Type == events.ConfigReloadFailed {
			changes, _ := json.Marshal(e.Payload.Changes)
			if _, err = tx.ExecContext(ctx, `INSERT INTO config_changes VALUES($1,$2,$3,$4)`, e.ID, e.Payload.Generation, string(e.Payload.Result), string(changes)); err != nil {
				return nil, databaseError(err)
			}
		}
		return func() { s.Bus.Notify(e) }, nil
	})
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
		from = micros(f.From)
	}
	if !f.Until.IsZero() {
		until = micros(f.Until)
	}
	afterTime, afterID := int64(-1<<63), ""
	if f.After != nil {
		afterTime, afterID = micros(f.After.At), f.After.ID
	}
	query := `SELECT event_id,type,occurred_at,coalesce(request_id,''),coalesce(app_name,''),coalesce(user_hash,''),anonymous,payload_json::text FROM events WHERE ($1='' OR app_name=$1) AND occurred_at>=$2 AND occurred_at<$3 AND (occurred_at,event_id)>($4,$5)`
	args := []any{f.App, from, until, afterTime, afterID}
	if len(f.Types) > 0 {
		var marks []string
		for _, kind := range f.Types {
			if !events.ValidType(kind) {
				return page, storage.ErrInvalid
			}
			args = append(args, string(kind))
			marks = append(marks, "$"+strconv.Itoa(len(args)))
		}
		query += ` AND type IN (` + strings.Join(marks, ",") + `)`
	}
	args = append(args, limit+1)
	query += ` ORDER BY occurred_at,event_id LIMIT $` + strconv.Itoa(len(args))
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return page, storage.ErrUnavailable
	}
	defer rows.Close()
	for rows.Next() {
		var e events.Event
		var at int64
		var payload, hash string
		var anonymous sql.NullBool
		if err = rows.Scan(&e.ID, &e.Type, &at, &e.RequestID, &e.App, &hash, &anonymous, &payload); err != nil {
			return page, storage.ErrUnavailable
		}
		if (e.Type == events.LoopCut || e.Type == events.SensitiveDataDetected) && hash != "" && anonymous.Valid {
			e.UserHash = hash
			flag := anonymous.Bool
			e.Anonymous = &flag
		}
		e.At = time.UnixMicro(at).UTC()
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

// CommitLocalCut writes a loop cut and its episode event together.
func (s *Store) CommitLocalCut(ctx context.Context, cut storage.LocalCut) error {
	r, e := cut.Record, cut.Event
	if err := storage.ValidateRecord(r); err != nil {
		return err
	}
	if err := e.ValidateLoopCut(); err != nil {
		return storage.ErrInvalid
	}
	if r.Outcome != "loop_cut" || r.AttemptIndex != 0 || e.App != r.AppName || e.UserHash != r.UserHash || *e.Anonymous != r.Anonymous {
		return storage.ErrInvalid
	}
	return s.commitCut(ctx, r, &e)
}

// CommitSensitiveCut writes a refusal of the sensitive data inspection.
func (s *Store) CommitSensitiveCut(ctx context.Context, cut storage.SensitiveCut) error {
	r, e := cut.Record, cut.Event
	if err := storage.ValidateRecord(r); err != nil {
		return err
	}
	if r.Outcome != "blocked" || r.AttemptIndex != 0 {
		return storage.ErrInvalid
	}
	switch {
	case r.ErrorCode == "sensitive_data_blocked" && e != nil:
		if e.ValidateSensitive() != nil || e.RequestID != r.RequestID || e.App != r.AppName || e.UserHash != r.UserHash || *e.Anonymous != r.Anonymous {
			return storage.ErrInvalid
		}
	case r.ErrorCode == "sensitive_data_scan_failed" && e == nil && r.Detections == nil:
	default:
		return storage.ErrInvalid
	}
	return s.commitCut(ctx, r, e)
}

func (s *Store) commitCut(ctx context.Context, r storage.Record, e *events.Event) error {
	data, _ := json.Marshal(r)
	quote, _ := json.Marshal(r.Quote)
	var payload []byte
	if e != nil {
		var err error
		if payload, err = json.Marshal(e.Payload); err != nil {
			return storage.ErrInvalid
		}
	}
	return s.inTx(ctx, func(tx *sql.Tx) (func(), error) {
		var old string
		err := tx.QueryRowContext(ctx, `SELECT record_json::text FROM requests WHERE request_id=$1 AND attempt_index=0`, r.RequestID).Scan(&old)
		switch {
		case err == nil:
			canonical, _, err := canonicalRecord(old)
			if err != nil {
				return nil, err
			}
			if canonical != string(data) {
				return nil, storage.ErrConflict
			}
		case err == sql.ErrNoRows:
			if err = insertRequest(ctx, tx, r, string(data), string(quote)); err != nil {
				return nil, err
			}
		default:
			return nil, databaseError(err)
		}
		if e == nil {
			return nil, nil
		}
		inserted, err := putEvent(ctx, tx, *e, string(payload))
		if err != nil || !inserted {
			return nil, err
		}
		event := *e
		return func() { s.Bus.Notify(event) }, nil
	})
}
