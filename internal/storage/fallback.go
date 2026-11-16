package storage

import (
	"context"
	"regexp"
	"strconv"
	"time"
)

// MaxAttempts bounds the upstream sends of one logical request: the initial
// model plus a chain of at most four.
const MaxAttempts = 5

// Operation is the durable session of a logical request that went past its
// first attempt. It never holds content, a prepared body, the snapshot or a
// credential: after a crash it only allows financial recovery, never a resend.
type Operation struct {
	RequestID  string
	AppName    string
	Subject    string
	Anonymous  bool
	Generation int64
	StartedAt  time.Time
}

// FallbackStore admits the later attempts of a logical request. The new
// attempt's journal entry, its reservation, its token rate event, the session
// and the fallback event commit in one transaction, and only after the previous
// attempt's reservation was settled or released.
type FallbackStore interface {
	AdmitContinuation(context.Context, Record, Reservation, Operation) (int64, error)
}

var fallbackModel = regexp.MustCompile(`^[\x21-\x7e]{1,256}$`)

// ValidFallbackCause reports whether a cause belongs to the closed set: 429, an
// HTTP status from 500 to 599, connection or timeout.
func ValidFallbackCause(cause string) bool {
	switch cause {
	case "429", "connection", "timeout":
		return true
	}
	if len(cause) != 3 {
		return false
	}
	status, err := strconv.Atoi(cause)
	return err == nil && status >= 500 && status <= 599
}

// ValidFallbackModel reports whether a model name may appear in fallback
// metadata: printable ASCII without spaces, 1 to 256 bytes.
func ValidFallbackModel(model string) bool { return fallbackModel.MatchString(model) }

// ValidateFallback holds the fallback and terminal fields to their domain.
func (r Record) ValidateFallback() error {
	if (r.FallbackFrom == "") != (r.FallbackCause == "") {
		return ErrInvalid
	}
	if r.FallbackFrom != "" && (r.AttemptIndex < 2 || r.AttemptIndex > MaxAttempts || r.Outcome != "fallback" ||
		!ValidFallbackModel(r.FallbackFrom) || !ValidFallbackCause(r.FallbackCause)) {
		return ErrInvalid
	}
	if s := r.TerminalStatus; s != nil && (*s < 100 || *s > 599) {
		return ErrInvalid
	}
	if (r.TerminalStatus != nil || r.TerminalErrorCode != "") && r.RequestCount != 1 {
		return ErrInvalid
	}
	return nil
}
