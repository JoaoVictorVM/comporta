package admin

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/storage"
	"github.com/JoaoVictorVM/comporta/internal/useridentity"
)

const (
	// MaxUsageDays is the longest period a usage query may cover.
	MaxUsageDays = 366
	// MaterializationDelay is the nominal delay before a completion is counted.
	MaterializationDelay = 2 * time.Second

	defaultUsageLimit = 100
	maxUsageLimit     = 1000
	maxCursorBytes    = 2048
)

var (
	sincePattern = regexp.MustCompile(`^[1-9][0-9]{0,2}d$`)
	dayPattern   = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)
)

// UsageQuery filters aggregated usage. UserID is the raw identifier, used only
// to compute each app's pseudonym and never returned.
type UsageQuery struct {
	App       string
	UserID    string
	HasUser   bool
	Anonymous bool
	Since     string
	From      string
	Through   string
	Limit     int
	Cursor    string
}

// UsageRow is one day of one app. Numbers are decimal strings, exact beyond
// the range of a JavaScript number.
type UsageRow struct {
	App          string `json:"app"`
	Day          string `json:"day"`
	Timezone     string `json:"timezone"`
	Requests     string `json:"requests"`
	Tokens       string `json:"tokens"`
	CostMicrousd string `json:"cost_microusd"`
	UsageMissing string `json:"usage_missing"`
}

// UsageTotals cover the whole filter, not only the page.
type UsageTotals struct {
	Requests     string `json:"requests"`
	Tokens       string `json:"tokens"`
	CostMicrousd string `json:"cost_microusd"`
}

// UsagePage is the answer to a usage query.
type UsagePage struct {
	ReferenceAt                 string      `json:"reference_at"`
	MaterializationDelaySeconds int         `json:"materialization_delay_seconds"`
	Rows                        []UsageRow  `json:"rows"`
	Totals                      UsageTotals `json:"totals"`
	NextCursor                  *string     `json:"next_cursor"`
}

type usageCursor struct {
	Version   int    `json:"v"`
	Day       string `json:"d"`
	Timezone  string `json:"z"`
	App       string `json:"a"`
	Reference int64  `json:"r"`
	Filter    string `json:"f"`
}

// newCursorKey is random per process: a restart invalidates every cursor.
func newCursorKey() []byte {
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	return key
}

func (s *Service) sign(payload []byte) string {
	mac := hmac.New(sha256.New, s.cursorKey)
	mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s *Service) open(cursor string) (usageCursor, bool) {
	var decoded usageCursor
	data, signature, ok := strings.Cut(cursor, ".")
	if !ok || len(cursor) > maxCursorBytes {
		return decoded, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(data)
	if err != nil {
		return decoded, false
	}
	given, err := base64.RawURLEncoding.DecodeString(signature)
	if err != nil {
		return decoded, false
	}
	mac := hmac.New(sha256.New, s.cursorKey)
	mac.Write(payload)
	if !hmac.Equal(given, mac.Sum(nil)) || json.Unmarshal(payload, &decoded) != nil || decoded.Version != 1 {
		return decoded, false
	}
	return decoded, true
}

// filterDigest binds a cursor to its filters without carrying the identifier.
func (s *Service) filterDigest(q UsageQuery) string {
	canonical, _ := json.Marshal([]any{q.App, q.HasUser, q.UserID, q.Anonymous, q.Since, q.From, q.Through, q.Limit})
	mac := hmac.New(sha256.New, s.cursorKey)
	mac.Write([]byte("filter\x00"))
	mac.Write(canonical)
	return hex.EncodeToString(mac.Sum(nil)[:16])
}

func parseDay(value string) (time.Time, bool) {
	if !dayPattern.MatchString(value) {
		return time.Time{}, false
	}
	day, err := time.Parse(time.DateOnly, value)
	return day, err == nil
}

// normalize validates the query and fills the defaults.
func normalize(q UsageQuery) (UsageQuery, int, bool) {
	if q.App != "" && !appName.MatchString(q.App) {
		return q, 0, false
	}
	if q.HasUser && (q.Anonymous || useridentity.ValidateID(q.UserID) != nil) {
		return q, 0, false
	}
	if q.Limit == 0 {
		q.Limit = defaultUsageLimit
	}
	if q.Limit < 1 || q.Limit > maxUsageLimit {
		return q, 0, false
	}
	if (q.From == "") != (q.Through == "") {
		return q, 0, false
	}
	if q.From != "" {
		if q.Since != "" {
			return q, 0, false
		}
		from, ok1 := parseDay(q.From)
		through, ok2 := parseDay(q.Through)
		if !ok1 || !ok2 || through.Before(from) || int(through.Sub(from).Hours()/24)+1 > MaxUsageDays {
			return q, 0, false
		}
		return q, 0, true
	}
	if q.Since == "" {
		q.Since = "7d"
	}
	if !sincePattern.MatchString(q.Since) {
		return q, 0, false
	}
	days, _ := strconv.Atoi(strings.TrimSuffix(q.Since, "d"))
	if days < 1 || days > MaxUsageDays {
		return q, 0, false
	}
	return q, days, true
}

// QueryUsage reads aggregated usage by app, user and period. Without a user
// selector it reads the app totals; with one, only that user's rows. Days are
// the civil days the usage was recorded under; since counts back from the
// reference instant in the app's current timezone, or in the historical
// timezone of each row for an app no longer configured.
func (s *Service) QueryUsage(ctx context.Context, query UsageQuery) (UsagePage, error) {
	q, days, ok := normalize(query)
	if !ok {
		return UsagePage{}, ErrInvalidInput
	}
	reference := s.now().UTC().Truncate(time.Second)
	var after *usageCursor
	digest := s.filterDigest(q)
	if q.Cursor != "" {
		cursor, valid := s.open(q.Cursor)
		if !valid || cursor.Filter != digest {
			return UsagePage{}, ErrInvalidInput
		}
		reference = time.Unix(cursor.Reference, 0).UTC()
		after = &cursor
	}
	selector := storage.UsageSelector{Scope: "app", From: q.From, Through: q.Through}
	if days > 0 {
		// A broad label window; each row is then checked in its own timezone.
		selector.From = reference.AddDate(0, 0, -days-1).Format(time.DateOnly)
		selector.Through = reference.AddDate(0, 0, 2).Format(time.DateOnly)
	}
	if q.HasUser || q.Anonymous {
		selector.Scope = "user"
		apps := []string{q.App}
		if q.App == "" {
			var err error
			if apps, err = s.apps(ctx); err != nil {
				return UsagePage{}, ErrStorageFailed
			}
		}
		for _, app := range apps {
			subject := storage.UsageSubject{App: app, Anonymous: q.Anonymous}
			if q.Anonymous {
				subject.Subject = s.identity.AnonymousHash(app).Hex()
			} else {
				hash, err := s.identity.LookupHash(app, q.UserID)
				if err != nil {
					return UsagePage{}, ErrInvalidInput
				}
				subject.Subject = hash.Hex()
			}
			selector.Subjects = append(selector.Subjects, subject)
		}
	} else if q.App != "" {
		selector.Subjects = []storage.UsageSubject{{App: q.App}}
	}
	rows, err := s.usage.QueryDaily(ctx, selector)
	if err != nil {
		s.logger.Error("usage query failed")
		return UsagePage{}, ErrStorageFailed
	}
	snapshot := s.source.Capture()
	page := UsagePage{ReferenceAt: reference.Format(time.RFC3339), MaterializationDelaySeconds: int(MaterializationDelay / time.Second), Rows: []UsageRow{}}
	var requests, tokens, cost big.Int
	for _, row := range rows {
		if days > 0 {
			zone := row.Timezone
			if app, ok := snapshot.App(row.AppName); ok {
				zone = app.Timezone
			}
			location, err := time.LoadLocation(zone)
			if err != nil {
				return UsagePage{}, ErrStorageFailed
			}
			local := reference.In(location)
			if row.Day < local.AddDate(0, 0, -days+1).Format(time.DateOnly) || row.Day > local.Format(time.DateOnly) {
				continue
			}
		}
		var rowTokens big.Int
		for _, n := range []int64{row.Input, row.Output, row.CacheWrite, row.CacheRead} {
			rowTokens.Add(&rowTokens, big.NewInt(n))
		}
		requests.Add(&requests, big.NewInt(row.Requests))
		tokens.Add(&tokens, &rowTokens)
		cost.Add(&cost, big.NewInt(int64(row.Cost)))
		if after != nil && !afterCursor(row, *after) {
			continue
		}
		if len(page.Rows) == q.Limit {
			if page.NextCursor == nil {
				last := page.Rows[len(page.Rows)-1]
				payload, _ := json.Marshal(usageCursor{Version: 1, Day: last.Day, Timezone: last.Timezone, App: last.App, Reference: reference.Unix(), Filter: digest})
				next := s.sign(payload)
				page.NextCursor = &next
			}
			continue
		}
		page.Rows = append(page.Rows, UsageRow{
			App: row.AppName, Day: row.Day, Timezone: row.Timezone, Requests: strconv.FormatInt(row.Requests, 10),
			Tokens: rowTokens.String(), CostMicrousd: strconv.FormatInt(int64(row.Cost), 10), UsageMissing: strconv.FormatInt(row.UsageMissing, 10),
		})
	}
	page.Totals = UsageTotals{Requests: requests.String(), Tokens: tokens.String(), CostMicrousd: cost.String()}
	return page, nil
}

// afterCursor orders rows by day, timezone and app, the order of the query.
func afterCursor(row storage.Daily, cursor usageCursor) bool {
	if row.Day != cursor.Day {
		return row.Day > cursor.Day
	}
	if row.Timezone != cursor.Timezone {
		return row.Timezone > cursor.Timezone
	}
	return row.AppName > cursor.App
}
