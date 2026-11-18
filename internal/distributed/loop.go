package distributed

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/loopguard"
	redisstore "github.com/JoaoVictorVM/comporta/internal/storage/redis"
)

// SnapshotSource provides the configuration in force.
type SnapshotSource interface {
	Capture() *config.Snapshot
}

// LoopGuard applies the loop rules to arrivals counted by every replica, so
// alternating replicas never splits a runaway agent's count. It keeps no
// content: signatures are wrapped in an HMAC with a key every replica derives
// from the shared identity secret.
type LoopGuard struct {
	redis  *redisstore.Store
	source SnapshotSource
	key    []byte
	logger *slog.Logger
}

// NewLoopGuard returns the shared guard.
func NewLoopGuard(redis *redisstore.Store, source SnapshotSource, key []byte, logger *slog.Logger) *LoopGuard {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &LoopGuard{redis: redis, source: source, key: key, logger: logger}
}

func (g *LoopGuard) failMode(app string) config.FailMode {
	if g.source == nil {
		return config.FailClosed
	}
	if a, ok := g.source.Capture().App(app); ok {
		return a.FailMode
	}
	return config.FailClosed
}

// Check counts one arrival. During an ordinary Redis outage an app under
// fail_open is let through uncounted, never against local counters taken for
// global ones; fail_closed refuses. A lost history quarantines every app,
// fail_open included, because it is not an outage but an unknown past.
func (g *LoopGuard) Check(req loopguard.Request) (loopguard.Decision, error) {
	p := req.Policy
	if req.Subject.App == "" || req.Subject.Hash == "" {
		return loopguard.Decision{}, loopguard.ErrInvalid
	}
	if !p.Enabled {
		return loopguard.Decision{Allowed: true}, nil
	}
	if p.SimilarRequests <= 0 || p.SimilarWindow <= 0 || p.RequestsPerMinute <= 0 || p.Cooldown <= 0 {
		return loopguard.Decision{}, loopguard.ErrInvalid
	}
	arrival := redisstore.LoopArrival{App: req.Subject.App, Subject: redisstore.SubjectKey(req.Subject.Hash, req.Subject.Anonymous), RequestID: req.RequestID,
		EventID: newEventID(), Policy: fmt.Sprintf("%d:%d:%d", p.SimilarRequests, p.SimilarWindow.Milliseconds(), p.RequestsPerMinute),
		Similar: p.SimilarRequests, Window: p.SimilarWindow, RPM: p.RequestsPerMinute, Cooldown: p.Cooldown}
	if req.HasSignature {
		mac := hmac.New(sha256.New, g.key)
		mac.Write(req.Signature[:])
		arrival.Signature = hex.EncodeToString(mac.Sum(nil)[:16])
	}
	ctx, cancel := context.WithTimeout(context.Background(), redisCall)
	defer cancel()
	episode, err := g.redis.LoopCheck(ctx, arrival)
	switch {
	case errors.Is(err, redisstore.ErrUnavailable):
		if g.failMode(req.Subject.App) == config.FailOpen {
			g.logger.Warn("loop guard degraded; arrival not counted")
			return loopguard.Decision{Allowed: true}, nil
		}
		return loopguard.Decision{}, err
	case errors.Is(err, redisstore.ErrLoopCapacity):
		return loopguard.Decision{}, loopguard.ErrCapacity
	case err != nil:
		return loopguard.Decision{}, err
	case episode == nil:
		return loopguard.Decision{Allowed: true}, nil
	}
	return loopguard.Decision{First: episode.First, Remaining: episode.Remaining, Episode: loopguard.Episode{
		EventID: episode.EventID, At: episode.At, RequestID: episode.RequestID, Reason: loopguard.Reason(episode.Reason),
		Count: episode.Count, Limit: episode.Limit, Window: episode.Window, Cooldown: episode.Cooldown}}, nil
}

// ForgetSubjects removes the loop state of deleted users.
func (g *LoopGuard) ForgetSubjects(subjects []loopguard.Subject) int {
	cleared := 0
	for _, s := range subjects {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		if g.redis.LoopForget(ctx, s.App, redisstore.SubjectKey(s.Hash, false)) == nil {
			cleared++
		}
		cancel()
	}
	return cleared
}

// Apps is empty: the shared state is found through the history of each app.
func (g *LoopGuard) Apps() []string { return nil }

func newEventID() string {
	var id [16]byte
	_, _ = rand.Read(id[:])
	return hex.EncodeToString(id[:])
}
