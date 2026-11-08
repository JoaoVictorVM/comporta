package sqlite

import (
	"context"
	"github.com/JoaoVictorVM/comporta/internal/storage"
	"time"
)

func (s *Store) Retain(ctx context.Context, cutoff time.Time) error {
	for _, table := range []struct{ name, column string }{{"requests", "finished_at"}, {"events", "occurred_at"}} {
		for {
			s.mu.Lock()
			result, err := s.db.ExecContext(ctx, `DELETE FROM `+table.name+` WHERE rowid IN(SELECT rowid FROM `+table.name+` WHERE `+table.column+` < ? LIMIT 1000)`, cutoff.UnixMicro())
			s.mu.Unlock()
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
	return nil
}

func (s *Store) Daily(ctx context.Context, f storage.Filter) ([]storage.Daily, error) {
	if f.AppName == "" || (f.UserHash != "" && !hexID.MatchString(f.UserHash)) {
		return nil, storage.ErrInvalid
	}
	scope, subject := "app", ""
	anonymous := false
	if f.UserHash != "" {
		scope, subject, anonymous = "user", f.UserHash, f.Anonymous
	}
	db := s.readDB
	if db == nil {
		db = s.db
	}
	rows, err := db.QueryContext(ctx, `SELECT day,timezone,app_name,scope,subject,anonymous,requests,attempts,input_tokens,output_tokens,cache_write_tokens,cache_read_tokens,usage_missing_count,recovered_count,cost_microusd FROM usage_daily WHERE app_name=? AND scope=? AND subject=? AND anonymous=? AND (?='' OR day>=?) AND (?='' OR day<=?) ORDER BY day,timezone`, f.AppName, scope, subject, anonymous, f.Since, f.Since, f.Until, f.Until)
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
	return out, rows.Err()
}
