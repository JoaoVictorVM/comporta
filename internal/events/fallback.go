package events

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// FallbackEventID derives the event of one admitted transition, so a retried
// write of the same admission can never become a second event.
func FallbackEventID(requestID string, attempt int) string {
	id := sha256.Sum256([]byte(fmt.Sprintf("fallback:%s:%d", requestID, attempt)))
	return hex.EncodeToString(id[:16])
}

// NewFallback builds the event of the next attempt admitted after an eligible
// failure. It means admission, not recovery: the new attempt may still fail.
// It carries configured model names and a closed cause, never a user or content.
func NewFallback(at time.Time, requestID, app string, attempt int, from, to, cause string) Event {
	class, code := FallbackCauseError(cause)
	return Event{
		ID: FallbackEventID(requestID, attempt), Type: Fallback, At: at.UTC(), RequestID: requestID, App: app,
		Payload: Payload{AttemptIndex: attempt, FromModel: from, ToModel: to, Cause: cause, ErrorClass: class, ErrorCode: code},
	}
}

// FallbackCauseError returns the error class and code a cause comes from.
// Both are empty for a value outside the closed set of causes.
func FallbackCauseError(cause string) (class, code string) {
	switch cause {
	case "429":
		return "rate_limit", "provider_rate_limited"
	case "529":
		return "overload", "provider_overloaded"
	case "connection":
		return "connection", "provider_unreachable"
	case "timeout":
		return "timeout", "provider_timeout"
	}
	status, err := strconv.Atoi(cause)
	if err != nil || len(cause) != 3 || status < 500 || status > 599 {
		return "", ""
	}
	return "upstream_5xx", "provider_error"
}

func fallbackCauseMatches(cause, class, code string) bool {
	wantClass, wantCode := FallbackCauseError(cause)
	return wantClass != "" && class == wantClass && code == wantCode
}

// validateFallbackFields keeps the transition fields on fallback events only.
// Older fallback events stored without them stay readable; NewFallback always
// sets all of them, and ValidateFallback holds new producers to that.
func (e Event) validateFallbackFields() error {
	has := e.Payload.FromModel != "" || e.Payload.ToModel != "" || e.Payload.Cause != ""
	if !has {
		return nil
	}
	if e.Type != Fallback || !modelName.MatchString(e.Payload.FromModel) || !modelName.MatchString(e.Payload.ToModel) ||
		!fallbackCauseMatches(e.Payload.Cause, e.Payload.ErrorClass, e.Payload.ErrorCode) {
		return errors.New("invalid fallback event payload")
	}
	return nil
}

// ValidateFallback holds a new fallback event to its full contract.
func (e Event) ValidateFallback() error {
	if err := e.Validate(); err != nil {
		return err
	}
	if e.Type != Fallback || e.RequestID == "" || e.App == "" || e.Payload.FromModel == "" || e.Payload.AttemptIndex < 2 || e.Payload.AttemptIndex > 5 ||
		e.ID != FallbackEventID(e.RequestID, e.Payload.AttemptIndex) || e.Payload.Outcome != "" || e.Payload.Cost != 0 {
		return errors.New("invalid fallback event")
	}
	return nil
}
