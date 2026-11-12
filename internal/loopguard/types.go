// Package loopguard cuts end users whose traffic looks like a runaway agent:
// the same signature repeated too often, or too many requests in a minute. It
// keeps only arrival instants and fixed-size digests in memory, never content.
package loopguard

import (
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/config"
)

// RateWindow is the fixed sliding window of the request-rate rule.
const RateWindow = 60 * time.Second

var (
	// ErrCapacity means the arrival would need memory beyond the configured
	// ceiling. Protected subjects are never evicted to make room.
	ErrCapacity = errors.New("loop guard capacity exhausted")
	// ErrStale means the policy came from a configuration generation older than
	// one already applied to the app. The caller recaptures and retries.
	ErrStale = errors.New("loop guard policy is older than the applied one")
	// ErrInvalid means the request or the policy cannot be evaluated.
	ErrInvalid = errors.New("invalid loop guard request")
)

// Subject is the unit of detection. Two keys of the same app share it; the
// model and the stream flag do not split it.
type Subject struct {
	App       string
	Hash      string
	Anonymous bool
}

func (s Subject) String() string { return "loopguard.Subject{redacted}" }

// Format keeps the pseudonym out of every %v and %+v.
func (s Subject) Format(state fmt.State, _ rune) { _, _ = fmt.Fprint(state, s.String()) }

func (s Subject) LogValue() slog.Value { return slog.StringValue(s.String()) }

// Policy is the loop guard configuration of one app at one generation.
type Policy struct {
	Generation        int64
	Enabled           bool
	SimilarRequests   int64
	SimilarWindow     time.Duration
	RequestsPerMinute int64
	Cooldown          time.Duration
}

// PolicyFor reads the policy of an app from a snapshot.
func PolicyFor(snapshot *config.Snapshot, app string) (Policy, bool) {
	if snapshot == nil {
		return Policy{}, false
	}
	cfg, ok := snapshot.App(app)
	if !ok {
		return Policy{}, false
	}
	return policyOf(cfg.LoopGuard, snapshot.Generation()), true
}

func policyOf(cfg config.LoopGuard, generation int64) Policy {
	return Policy{
		Generation: generation, Enabled: cfg.Enabled,
		SimilarRequests: cfg.SimilarRequests, SimilarWindow: cfg.SimilarWindow,
		RequestsPerMinute: cfg.RequestsPerMinute, Cooldown: cfg.Cooldown,
	}
}

func (p Policy) valid() bool {
	return !p.Enabled || (p.SimilarRequests > 0 && p.SimilarWindow > 0 && p.RequestsPerMinute > 0 && p.Cooldown > 0)
}

// sameDetection reports whether two policies count arrivals the same way.
// Cooldown is not part of it: a new cooldown only applies to new episodes.
func (p Policy) sameDetection(o Policy) bool {
	return p.SimilarRequests == o.SimilarRequests && p.SimilarWindow == o.SimilarWindow && p.RequestsPerMinute == o.RequestsPerMinute
}

// Request is one arrival at the guard. It carries a digest, never content.
type Request struct {
	Subject   Subject
	RequestID string
	// Signature is the digest of the last user message and last tool result.
	// Without it only the rate rule applies.
	Signature    [32]byte
	HasSignature bool
	Policy       Policy
}

// Reason is why an episode started.
type Reason string

const (
	ReasonSimilar Reason = "similar_requests"
	ReasonRate    Reason = "request_rate"
)

// Episode is the frozen metadata of one cut. Every retry during the cooldown
// sees the same values, so the event built from it stays idempotent.
type Episode struct {
	EventID   string
	At        time.Time
	RequestID string
	Reason    Reason
	// Count is the arrival that started the cut: limit plus one.
	Count    int64
	Limit    int64
	Window   time.Duration
	Cooldown time.Duration
}

// Decision is the answer to one arrival.
type Decision struct {
	Allowed bool
	// First is true only for the arrival that started the episode.
	First   bool
	Episode Episode
	// Remaining is the cooldown left at this arrival.
	Remaining time.Duration
}

func (d Decision) String() string {
	if d.Allowed {
		return "loopguard.Decision{allowed}"
	}
	return fmt.Sprintf("loopguard.Decision{cut %s}", d.Episode.Reason)
}
