package proxy

import (
	"net/http"

	"github.com/JoaoVictorVM/comporta/internal/telemetry"
)

func (r Route) operation() telemetry.Operation {
	switch r {
	case RouteCountTokens:
		return telemetry.OperationCountTokens
	case RouteModels:
		return telemetry.OperationModels
	}
	return telemetry.OperationGeneration
}

// observe ends the request's telemetry with its terminal outcome. It runs
// after finish, so the conclusion of the last attempt is already known.
func (o *operation) observe(r *http.Request) {
	if o.tel == nil {
		return
	}
	o.tel.End(o.outcome(r))
}

// outcome applies the precedence canceled, error, loop_cut, blocked, cached,
// fallback, downgraded, ok. A stream that fails after its 200 was sent is an
// error or a cancellation all the same.
func (o *operation) outcome(r *http.Request) telemetry.RequestEnd {
	end := telemetry.RequestEnd{}
	if o.w.status != 0 {
		end.Status = telemetry.Some(o.w.status)
	}
	var code Code
	var detail *LimitDetail
	if f := o.w.failure; f != nil {
		code, detail = f.Code, o.w.limit
	}
	if o.terminal != nil {
		code = o.terminal.Code
	}
	var result *AttemptResult
	if o.attempt != nil {
		result = &o.attempt.result
	}
	if code != "" {
		end.Block = blockOf(code, detail)
	} else if o.unauthenticated {
		end.Block = authenticationBlock(o.w.status)
	}
	switch {
	case code == CodeClientDisconnected || (result != nil && result.ClientDisconnected) || (!o.w.committed && r.Context().Err() != nil):
		end.Outcome = telemetry.OutcomeCanceled
	case result != nil && result.ErrorCode != "" && result.ErrorCode != CodeClientDisconnected:
		end.Outcome, end.ErrorType = telemetry.OutcomeError, string(result.ErrorCode)
	case end.Block.Type == telemetry.BlockLoop:
		end.Outcome = telemetry.OutcomeLoopCut
	case end.Block.Type != "":
		end.Outcome = telemetry.OutcomeBlocked
	case code != "":
		end.Outcome, end.ErrorType = telemetry.OutcomeError, string(code)
	case end.Status.Known && end.Status.Value >= 400:
		end.Outcome = telemetry.OutcomeError
	case o.cacheHit:
		end.Outcome = telemetry.OutcomeCached
	case o.fallbacks > 0:
		end.Outcome = telemetry.OutcomeFallback
	case o.downgraded:
		end.Outcome = telemetry.OutcomeDowngraded
	default:
		end.Outcome = telemetry.OutcomeOK
	}
	if end.Outcome == telemetry.OutcomeCanceled {
		end.Block = telemetry.Block{}
	}
	return end
}

// blockOf classifies a terminal refusal by its primary reason. Failures of
// the gateway's own infrastructure are errors, never blocks.
func blockOf(code Code, detail *LimitDetail) telemetry.Block {
	scope := telemetry.ScopeApp
	if detail != nil && detail.Scope == string(telemetry.ScopeUser) {
		scope = telemetry.ScopeUser
	}
	switch code {
	case CodeRateLimited:
		return telemetry.Block{Type: telemetry.BlockRate, Scope: scope}
	case CodeConcurrencyExceeded:
		return telemetry.Block{Type: telemetry.BlockConcurrency, Scope: scope}
	case CodeBudgetExceeded:
		return telemetry.Block{Type: telemetry.BlockBudget, Scope: scope}
	case CodeHardCapExceeded:
		return telemetry.Block{Type: telemetry.BlockHardCap, Scope: scope}
	case CodeBudgetReserved:
		return telemetry.Block{Type: telemetry.BlockReservation, Scope: scope}
	case CodeLoopDetected:
		return telemetry.Block{Type: telemetry.BlockLoop, Scope: telemetry.ScopeUser}
	case CodeSensitiveDataBlocked:
		return telemetry.Block{Type: telemetry.BlockSensitive, Scope: telemetry.ScopeRequest}
	case CodeAppPaused:
		return telemetry.Block{Type: telemetry.BlockPaused, Scope: telemetry.ScopeApp}
	case CodeUserMaintenance:
		return telemetry.Block{Type: telemetry.BlockPaused, Scope: telemetry.ScopeUser}
	case CodeInvalidRequest, CodeModelUnknown, CodeModelNotAllowed, CodeProviderFormatMismatch, CodeUnsupportedMediaType,
		CodeRequestTooLarge, CodeUserRequired, CodeUserInvalid, CodeUserEmailNotAllowed, CodeUserDocumentNotAllowed:
		return telemetry.Block{Type: telemetry.BlockValidation, Scope: telemetry.ScopeRequest}
	}
	return telemetry.Block{}
}

// authenticationBlock classifies what the key check answered. An unavailable
// key store is an error, not a refusal.
func authenticationBlock(status int) telemetry.Block {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests:
		return telemetry.Block{Type: telemetry.BlockAuthentication, Scope: telemetry.ScopeRequest}
	}
	return telemetry.Block{}
}
