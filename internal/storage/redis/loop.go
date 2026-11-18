package redis

import (
	"context"
	_ "embed"
	"errors"
	"strconv"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

//go:embed scripts/loop.lua
var loopLua string

var loopScript = goredis.NewScript(integersLua + "\n" + loopLua)

// ErrLoopQuarantine means the loop history was lost with the projection and
// generations wait until it could no longer matter.
var ErrLoopQuarantine = errors.New("loop guard history being rebuilt")

// ErrLoopCapacity means the shared loop guard tracks as many subjects as it
// may; protected subjects are never evicted to make room.
var ErrLoopCapacity = errors.New("loop guard capacity exhausted")

// LoopMaxSubjects bounds the subjects tracked per cluster.
const LoopMaxSubjects = 100_000

// LoopArrival is one arrival at the shared loop guard.
type LoopArrival struct {
	App, Subject string
	RequestID    string
	EventID      string
	// Signature is already an HMAC of the request signature, hex encoded.
	Signature string
	Policy    string
	Similar   int64
	Window    time.Duration
	RPM       int64
	Cooldown  time.Duration
}

// LoopEpisode is the frozen episode of a cut.
type LoopEpisode struct {
	First     bool
	Remaining time.Duration
	EventID   string
	At        time.Time
	RequestID string
	Reason    string
	Count     int64
	Limit     int64
	Window    time.Duration
	Cooldown  time.Duration
}

// LoopCheck counts an arrival or answers the episode in force. A nil episode
// means the arrival is allowed.
func (s *Store) LoopCheck(ctx context.Context, a LoopArrival) (*LoopEpisode, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	k := s.Keys()
	subject := k.Loop(a.App, a.Subject)
	signature := subject + ":s:" + a.Signature
	hasSig := "0"
	if a.Signature != "" {
		hasSig = "1"
	}
	keys := []string{k.Meta(), subject, subject + ":v", signature, k.LoopCount()}
	args := []any{s.epoch(), a.RequestID, a.EventID, a.Policy, a.Similar, a.Window.Milliseconds(), a.RPM, a.Cooldown.Milliseconds(),
		hasSig, a.RequestID, LoopMaxSubjects, int64(60_000)}
	raw, err := loopScript.Run(ctx, s.client, keys, args...).Result()
	if err != nil {
		return nil, classify(err)
	}
	out := strs(raw)
	if len(out) == 0 {
		return nil, ErrIntegrity
	}
	switch out[0] {
	case "ok":
		return nil, nil
	case "fenced":
		return nil, ErrFenced
	case "quarantine":
		return nil, ErrLoopQuarantine
	case "capacity":
		return nil, ErrLoopCapacity
	case "cut":
		if len(out) != 11 {
			return nil, ErrIntegrity
		}
		n := func(i int) int64 { v, _ := strconv.ParseInt(out[i], 10, 64); return v }
		return &LoopEpisode{First: out[1] == "1", Remaining: time.Duration(n(2)) * time.Millisecond, EventID: out[3],
			At: msTime(n(4)), RequestID: out[5], Reason: out[6], Count: n(7), Limit: n(8),
			Window: time.Duration(n(9)) * time.Millisecond, Cooldown: time.Duration(n(10)) * time.Millisecond}, nil
	}
	return nil, ErrIntegrity
}

// LoopForget removes the loop state of subjects, after their deletion.
func (s *Store) LoopForget(ctx context.Context, app, subject string) error {
	k := s.Keys()
	key := k.Loop(app, subject)
	if err := s.client.Del(ctx, key, key+":v").Err(); err != nil {
		return classify(err)
	}
	var cursor uint64
	for {
		keys, next, err := s.client.Scan(ctx, cursor, key+":s:*", 200).Result()
		if err != nil {
			return classify(err)
		}
		if len(keys) > 0 {
			if err := s.client.Unlink(ctx, keys...).Err(); err != nil {
				return classify(err)
			}
		}
		if next == 0 {
			break
		}
		cursor = next
	}
	return classify(s.client.ZRem(ctx, k.LoopCount(), key).Err())
}

// ForgetSubject removes every projection key of a deleted user in one app.
// App buckets and the app's slots stay; the user's own state goes.
func (s *Store) ForgetSubject(ctx context.Context, app, subject string) error {
	k := s.Keys()
	pipe := s.client.TxPipeline()
	pipe.Del(ctx, k.Budget(app, subject), k.Rate(app, subject), k.Slots(app, subject))
	pipe.SRem(ctx, k.Subjects(app), subject)
	pipe.Incr(ctx, k.Version(app))
	if _, err := pipe.Exec(ctx); err != nil {
		return classify(err)
	}
	return s.LoopForget(ctx, app, subject)
}
