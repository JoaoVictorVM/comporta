package postgres

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/storage"
)

func scanDaily(rows *sql.Rows) ([]storage.Daily, error) {
	var out []storage.Daily
	for rows.Next() {
		var d storage.Daily
		var cost, cacheSaved, downgradeSaved int64
		if err := rows.Scan(&d.Day, &d.Timezone, &d.AppName, &d.Scope, &d.Subject, &d.Anonymous, &d.Requests, &d.Attempts, &d.Input, &d.Output, &d.CacheWrite, &d.CacheRead, &d.UsageMissing, &d.Recovered, &cost, &cacheSaved, &downgradeSaved); err != nil {
			return nil, storage.ErrUnavailable
		}
		d.Cost, d.CacheSaved, d.DowngradeSaved = configMicrousd(cost), configMicrousd(cacheSaved), configMicrousd(downgradeSaved)
		out = append(out, d)
	}
	if rows.Err() != nil {
		return nil, storage.ErrUnavailable
	}
	return out, nil
}

const dailyColumns = `day,timezone,app_name,scope,subject,anonymous,requests,attempts,input_tokens,output_tokens,cache_write_tokens,cache_read_tokens,usage_missing_count,recovered_count,cost_microusd,cache_saved_microusd,downgrade_saved_microusd`

func (s *Store) Daily(ctx context.Context, f storage.Filter) ([]storage.Daily, error) {
	if f.AppName == "" || (f.UserHash != "" && !storage.HexID.MatchString(f.UserHash)) {
		return nil, storage.ErrInvalid
	}
	scope, subject, anonymous := "app", "", false
	if f.UserHash != "" {
		scope, subject, anonymous = "user", f.UserHash, f.Anonymous
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+dailyColumns+` FROM usage_daily WHERE app_name=$1 AND scope=$2 AND subject=$3 AND anonymous=$4 AND ($5='' OR day>=$5) AND ($6='' OR day<=$6) ORDER BY day,timezone`,
		f.AppName, scope, subject, anonymous, f.Since, f.Until)
	if err != nil {
		return nil, storage.ErrUnavailable
	}
	defer rows.Close()
	return scanDaily(rows)
}

func (s *Store) Apps(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT app_name FROM usage_daily ORDER BY app_name`)
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
	if f.UserHash != "" && (f.AppName == "" || !storage.HexID.MatchString(f.UserHash)) {
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
		from = micros(f.From)
	}
	if !f.Until.IsZero() {
		until = micros(f.Until)
	}
	afterTime := int64(-1 << 63)
	if !after.StartedAt.IsZero() {
		afterTime = micros(after.StartedAt)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT record_json::text,started_at,request_id,attempt_index FROM requests WHERE ($1='' OR app_name=$1) AND ($2='' OR (user_hash=$2 AND anonymous=$3)) AND started_at>=$4 AND started_at<$5 AND (started_at,request_id,attempt_index)>($6,$7,$8) ORDER BY started_at,request_id,attempt_index LIMIT $9`,
		f.AppName, f.UserHash, f.Anonymous, from, until, afterTime, after.RequestID, after.AttemptIndex, limit+1)
	if err != nil {
		return page, storage.ErrUnavailable
	}
	defer rows.Close()
	for rows.Next() {
		var data string
		var at int64
		var cursor storage.RecordCursor
		if err = rows.Scan(&data, &at, &cursor.RequestID, &cursor.AttemptIndex); err != nil {
			return page, storage.ErrUnavailable
		}
		if len(page.Records) == limit {
			last := page.Records[len(page.Records)-1]
			page.Next = &storage.RecordCursor{StartedAt: last.StartedAt, RequestID: last.RequestID, AttemptIndex: last.AttemptIndex}
			break
		}
		_, record, err := canonicalRecord(data)
		if err != nil {
			return page, err
		}
		page.Records = append(page.Records, record)
	}
	return page, rows.Err()
}

// QueryDaily reads one scope in a single repeatable-read transaction, so a page
// and its totals see the same state on any replica.
func (s *Store) QueryDaily(ctx context.Context, selector storage.UsageSelector) ([]storage.Daily, error) {
	if selector.Scope != "app" && selector.Scope != "user" {
		return nil, storage.ErrInvalid
	}
	if selector.Scope == "user" && len(selector.Subjects) == 0 {
		return nil, nil
	}
	query := `SELECT ` + dailyColumns + ` FROM usage_daily WHERE scope=$1 AND day>=$2 AND day<=$3`
	args := []any{selector.Scope, selector.From, selector.Through}
	if len(selector.Subjects) > 0 {
		var clauses []string
		for _, subject := range selector.Subjects {
			if !storage.RecordName.MatchString(subject.App) || (selector.Scope == "user" && !storage.HexID.MatchString(subject.Subject)) {
				return nil, storage.ErrInvalid
			}
			n := len(args)
			clauses = append(clauses, `(app_name=$`+strconv.Itoa(n+1)+` AND subject=$`+strconv.Itoa(n+2)+` AND anonymous=$`+strconv.Itoa(n+3)+`)`)
			args = append(args, subject.App, subject.Subject, subject.Anonymous)
		}
		query += ` AND (` + strings.Join(clauses, ` OR `) + `)`
	}
	query += ` ORDER BY day,timezone,app_name`
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return nil, storage.ErrUnavailable
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, storage.ErrUnavailable
	}
	defer rows.Close()
	return scanDaily(rows)
}

// Retain deletes detail rows older than the cutoff in batches. Aggregates,
// open journal entries, reservations and ledger rows needed by recovery stay.
func (s *Store) Retain(ctx context.Context, cutoff time.Time) error {
	for _, table := range []struct{ name, key, column string }{
		{"requests", "request_id,attempt_index", "finished_at"},
		{"events", "event_id", "occurred_at"},
	} {
		for {
			result, err := s.db.ExecContext(ctx, `DELETE FROM `+table.name+` WHERE (`+table.key+`) IN (SELECT `+table.key+` FROM `+table.name+` WHERE `+table.column+` < $1 LIMIT 1000)`, micros(cutoff))
			if err != nil {
				return databaseError(err)
			}
			n, err := result.RowsAffected()
			if err != nil {
				return databaseError(err)
			}
			if n < 1000 {
				break
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
		}
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM request_operations WHERE state='closed' AND materialized AND finished_at < $1`, micros(cutoff)); err != nil {
		return databaseError(err)
	}
	now := nowMicros()
	for _, q := range []string{`DELETE FROM admin_sessions WHERE expires_at <= $1`, `DELETE FROM auth_windows WHERE expires_at <= $1`} {
		if _, err := s.db.ExecContext(ctx, q, now); err != nil {
			return databaseError(err)
		}
	}
	return nil
}
