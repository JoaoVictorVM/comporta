package sqlite

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/storage"
)

func (s *Store) Apps(ctx context.Context) ([]string, error) {
	db := s.readDB
	if db == nil {
		db = s.db
	}
	rows, err := db.QueryContext(ctx, `SELECT DISTINCT app_name FROM usage_daily ORDER BY app_name`)
	if err != nil {
		return nil, storage.ErrUnavailable
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var app string
		if err = rows.Scan(&app); err != nil {
			return nil, storage.ErrUnavailable
		}
		out = append(out, app)
	}
	return out, rows.Err()
}

func (s *Store) Records(ctx context.Context, f storage.RecordFilter) (storage.RecordPage, error) {
	var page storage.RecordPage
	if f.UserHash != "" && (f.AppName == "" || !hexID.MatchString(f.UserHash)) {
		return page, storage.ErrInvalid
	}
	limit := f.Limit
	if limit == 0 {
		limit = 100
	}
	if limit < 1 || limit > 1000 {
		return page, storage.ErrInvalid
	}
	var after storage.RecordCursor
	if f.After != nil {
		after = *f.After
	}
	from, until := int64(-1<<63), int64(1<<63-1)
	if !f.From.IsZero() {
		from = f.From.UnixMicro()
	}
	if !f.Until.IsZero() {
		until = f.Until.UnixMicro()
	}
	afterTime := int64(-1 << 63)
	if !after.StartedAt.IsZero() {
		afterTime = after.StartedAt.UnixMicro()
	}
	db := s.readDB
	if db == nil {
		db = s.db
	}
	rows, err := db.QueryContext(ctx, `SELECT record_json,started_at,request_id,attempt_index FROM requests WHERE (?='' OR app_name=?) AND (?='' OR (user_hash=? AND anonymous=?)) AND started_at>=? AND started_at<? AND (started_at,request_id,attempt_index)>(?,?,?) ORDER BY started_at,request_id,attempt_index LIMIT ?`, f.AppName, f.AppName, f.UserHash, f.UserHash, f.Anonymous, from, until, afterTime, after.RequestID, after.AttemptIndex, limit+1)
	if err != nil {
		return page, storage.ErrUnavailable
	}
	defer rows.Close()
	for rows.Next() {
		var data string
		var micros int64
		var cursor storage.RecordCursor
		if err = rows.Scan(&data, &micros, &cursor.RequestID, &cursor.AttemptIndex); err != nil {
			return page, storage.ErrUnavailable
		}
		cursor.StartedAt = time.UnixMicro(micros).UTC()
		if len(page.Records) == limit {
			last := page.Records[len(page.Records)-1]
			page.Next = &storage.RecordCursor{StartedAt: last.StartedAt, RequestID: last.RequestID, AttemptIndex: last.AttemptIndex}
			break
		}
		var record storage.Record
		if err = decodeRecord(data, &record); err != nil {
			return page, err
		}
		page.Records = append(page.Records, record)
	}
	return page, rows.Err()
}

// QueryDaily reads the daily aggregates of one scope in a single read
// transaction, so a page and its totals come from the same state. The app and
// user scopes are never mixed: summing both would count usage twice.
func (s *Store) QueryDaily(ctx context.Context, selector storage.UsageSelector) ([]storage.Daily, error) {
	if selector.Scope != "app" && selector.Scope != "user" {
		return nil, storage.ErrInvalid
	}
	if selector.Scope == "user" && len(selector.Subjects) == 0 {
		return nil, nil
	}
	query := `SELECT day,timezone,app_name,scope,subject,anonymous,requests,attempts,input_tokens,output_tokens,cache_write_tokens,cache_read_tokens,usage_missing_count,recovered_count,cost_microusd FROM usage_daily WHERE scope=? AND day>=? AND day<=?`
	args := []any{selector.Scope, selector.From, selector.Through}
	if len(selector.Subjects) > 0 {
		var clauses []string
		for _, subject := range selector.Subjects {
			if !recordName.MatchString(subject.App) || (selector.Scope == "user" && !hexID.MatchString(subject.Subject)) {
				return nil, storage.ErrInvalid
			}
			clauses = append(clauses, `(app_name=? AND subject=? AND anonymous=?)`)
			args = append(args, subject.App, subject.Subject, subject.Anonymous)
		}
		query += ` AND (` + strings.Join(clauses, ` OR `) + `)`
	}
	query += ` ORDER BY day,timezone,app_name`
	db := s.readDB
	if db == nil {
		db = s.db
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, storage.ErrUnavailable
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, storage.ErrUnavailable
	}
	defer rows.Close()
	var out []storage.Daily
	for rows.Next() {
		var d storage.Daily
		if err = rows.Scan(&d.Day, &d.Timezone, &d.AppName, &d.Scope, &d.Subject, &d.Anonymous, &d.Requests, &d.Attempts, &d.Input, &d.Output, &d.CacheWrite, &d.CacheRead, &d.UsageMissing, &d.Recovered, &d.Cost); err != nil {
			return nil, storage.ErrUnavailable
		}
		out = append(out, d)
	}
	if rows.Err() != nil {
		return nil, storage.ErrUnavailable
	}
	return out, nil
}
