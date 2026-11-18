package redis

import (
	"strconv"
	"strings"

	"github.com/JoaoVictorVM/comporta/internal/limits"
	"github.com/JoaoVictorVM/comporta/internal/storage"
)

// Keys builds the names of one cluster epoch. Every component is a validated
// app name, a hex pseudonym, a hex request ID or a digit string, so the colon
// separator can never be ambiguous. No key holds a raw ID, an IP or content.
type Keys struct {
	prefix string
}

// NewKeys returns the namespace comporta:<cluster>:<epoch>:.
func NewKeys(cluster, epoch string) Keys {
	return Keys{prefix: "comporta:" + cluster + ":" + epoch + ":"}
}

func (k Keys) Prefix() string                 { return k.prefix }
func (k Keys) Meta() string                   { return k.prefix + "meta" }
func (k Keys) Pause() string                  { return k.prefix + "pause" }
func (k Keys) Version(app string) string      { return k.prefix + "v:" + app }
func (k Keys) Zone(app string) string         { return k.prefix + "z:" + app }
func (k Keys) Subjects(app string) string     { return k.prefix + "s:" + app }
func (k Keys) Owned(instance string) string   { return k.prefix + "o:" + instance }
func (k Keys) Pending() string                { return k.prefix + "pending" }
func (k Keys) Owners() string                 { return k.prefix + "owners" }
func (k Keys) Loop(app, subj string) string   { return k.prefix + "l:" + app + ":" + subj }
func (k Keys) LoopCount() string              { return k.prefix + "lcount" }
func (k Keys) Budget(app, subj string) string { return k.prefix + "b:" + app + ":" + subj }
func (k Keys) Rate(app, subj string) string   { return k.prefix + "r:" + app + ":" + subj }
func (k Keys) Slots(app, subj string) string  { return k.prefix + "c:" + app + ":" + subj }
func (k Keys) Receipt(request string, attempt int) string {
	return k.prefix + "t:" + request + ":" + strconv.Itoa(attempt)
}

// AppSubject is the subject key of the app scope.
const AppSubject = "-"

// SubjectKey encodes a user subject: its pseudonym and the anonymous flag.
func SubjectKey(hash string, anonymous bool) string {
	if hash == "" {
		return AppSubject
	}
	if anonymous {
		return hash + ":1"
	}
	return hash + ":0"
}

// ParseSubjectKey is the inverse of SubjectKey.
func ParseSubjectKey(key string) (hash string, anonymous bool, ok bool) {
	if key == AppSubject {
		return "", false, true
	}
	hash, flag, found := strings.Cut(key, ":")
	if !found || !storage.HexID.MatchString(hash) || (flag != "0" && flag != "1") {
		return "", false, false
	}
	return hash, flag == "1", true
}

// BucketField is the field prefix of one calendar window inside a subject hash.
func BucketField(dimension string, start, end int64) string {
	return dimension + "|" + strconv.FormatInt(start, 10) + "|" + strconv.FormatInt(end, 10)
}

// parseBuckets decodes a subject hash into buckets. Values are parsed as int64
// in Go, never through a float.
func parseBuckets(app string, subject limits.Subject, fields []string) ([]limits.SharedBucket, error) {
	scope := "app"
	if subject.Hash != "" {
		scope = "user"
	}
	type acc struct {
		b       limits.SharedBucket
		present bool
	}
	byPrefix := map[string]*acc{}
	for i := 0; i+1 < len(fields); i += 2 {
		name, value := fields[i], fields[i+1]
		cut := strings.LastIndexByte(name, '|')
		if cut < 0 {
			return nil, storage.ErrInvalid
		}
		prefix, kind := name[:cut], name[cut+1:]
		parts := strings.Split(prefix, "|")
		if len(parts) != 3 || !storage.ValidDimension(parts[0]) {
			return nil, storage.ErrInvalid
		}
		start, err1 := strconv.ParseInt(parts[1], 10, 64)
		end, err2 := strconv.ParseInt(parts[2], 10, 64)
		if err1 != nil || err2 != nil || end <= start {
			return nil, storage.ErrInvalid
		}
		a := byPrefix[prefix]
		if a == nil {
			a = &acc{b: limits.SharedBucket{BudgetBucket: storage.BudgetBucket{AppName: app, Scope: scope, Subject: subject.Hash, Anonymous: subject.Anonymous,
				Dimension: parts[0], WindowStart: start, WindowEnd: end}}}
			byPrefix[prefix] = a
		}
		switch kind {
		case "s", "r":
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil || n < 0 {
				return nil, storage.ErrInvalid
			}
			if kind == "s" {
				a.b.Spent = n
			} else {
				a.b.Reserved = n
			}
		case "u":
			a.b.Unknown = value == "1"
		default:
			return nil, storage.ErrInvalid
		}
	}
	out := make([]limits.SharedBucket, 0, len(byPrefix))
	for _, a := range byPrefix {
		out = append(out, a.b)
	}
	return out, nil
}

func parseRates(fields []string) ([]limits.SharedRate, error) {
	out := make([]limits.SharedRate, 0, len(fields)/2)
	for i := 0; i+1 < len(fields); i += 2 {
		request, attempt, ok := strings.Cut(fields[i], "|")
		if !ok || !storage.HexID.MatchString(request) {
			return nil, storage.ErrInvalid
		}
		index, err := strconv.Atoi(attempt)
		if err != nil || index < 1 || index > storage.MaxAttempts {
			return nil, storage.ErrInvalid
		}
		parts := strings.Split(fields[i+1], "|")
		if len(parts) != 3 {
			return nil, storage.ErrInvalid
		}
		at, err1 := strconv.ParseInt(parts[0], 10, 64)
		tokens, err2 := strconv.ParseInt(parts[1], 10, 64)
		if err1 != nil || err2 != nil || tokens < 0 {
			return nil, storage.ErrInvalid
		}
		out = append(out, limits.SharedRate{RequestID: request, Attempt: index, At: msTime(at), Tokens: tokens, Active: parts[2] == "1"})
	}
	return out, nil
}
