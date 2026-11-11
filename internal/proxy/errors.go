package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/auth"
	"github.com/JoaoVictorVM/comporta/internal/runtimecontrol"
)

// Code is the public comporta_code of a failure.
type Code string

const (
	CodeEndpointNotAllowed      Code = "endpoint_not_allowed"
	CodeHostNotAllowed          Code = "host_not_allowed"
	CodeInvalidRequest          Code = "invalid_request"
	CodeModelUnknown            Code = "model_unknown"
	CodeProviderFormatMismatch  Code = "provider_format_mismatch"
	CodeUnsupportedMediaType    Code = "unsupported_media_type"
	CodeRequestTooLarge         Code = "request_too_large"
	CodeProviderUnreachable     Code = "provider_unreachable"
	CodeProviderTimeout         Code = "provider_timeout"
	CodeProviderRedirect        Code = "provider_redirect"
	CodeInvalidUpstreamResponse Code = "invalid_upstream_response"
	CodeProviderRateLimited     Code = "provider_rate_limited"
	CodeProviderOverloaded      Code = "provider_overloaded"
	CodeProviderError           Code = "provider_error"
	CodeInternalError           Code = "internal_error"
	CodeClientDisconnected      Code = "client_disconnected"
	CodeUserRequired            Code = "user_required"
	CodeUserInvalid             Code = "user_invalid"
	CodeUserEmailNotAllowed     Code = "user_email_not_allowed"
	CodeUserDocumentNotAllowed  Code = "user_document_not_allowed"
	CodeRateLimited             Code = "rate_limited"
	CodeConcurrencyExceeded     Code = "concurrency_exceeded"
	CodeBudgetExceeded          Code = "budget_exceeded"
	CodeBudgetReserved          Code = "budget_reserved"
	CodeHardCapExceeded         Code = "hard_cap_exceeded"
	CodeLimitsUnavailable       Code = "limits_unavailable"
	CodeUsageStorageUnavailable Code = "usage_storage_unavailable"
	CodeAppPaused               Code = "app_paused"
	CodeUserMaintenance         Code = "user_maintenance"
)

// ErrorClass groups failures for internal decisions, such as a later fallback.
type ErrorClass string

const (
	ClassInvalidRequest ErrorClass = "invalid_request"
	ClassAuthentication ErrorClass = "authentication"
	ClassRateLimit      ErrorClass = "rate_limit"
	ClassOverload       ErrorClass = "overload"
	ClassUpstream5xx    ErrorClass = "upstream_5xx"
	ClassConnection     ErrorClass = "connection"
	ClassTimeout        ErrorClass = "timeout"
	ClassProtocol       ErrorClass = "protocol"
	ClassClientCancel   ErrorClass = "client_cancel"
	ClassInternal       ErrorClass = "internal"
)

const statusOverloaded = 529

// Failure is a sanitized error. Its message is built by the gateway from fixed
// vocabulary, never from a provider body, a request body or a raw network error.
type Failure struct {
	Status  int
	Code    Code
	Class   ErrorClass
	Message string
}

func invalidRequest(message string) Failure {
	return Failure{http.StatusBadRequest, CodeInvalidRequest, ClassInvalidRequest, message}
}

// gateFailure answers a refusal of the operator overrides. A pause has no known
// renewal time, so no Retry-After is promised.
func gateFailure(err error, app string) Failure {
	switch {
	case errors.Is(err, runtimecontrol.ErrPaused):
		return Failure{http.StatusServiceUnavailable, CodeAppPaused, ClassInternal, fmt.Sprintf("app %s is paused by an operator", app)}
	case errors.Is(err, runtimecontrol.ErrMaintenance):
		return Failure{http.StatusServiceUnavailable, CodeUserMaintenance, ClassInternal, "this user's data is being changed by an operator; retry in a few seconds"}
	}
	return internalFailure()
}

func internalFailure() Failure {
	return Failure{http.StatusInternalServerError, CodeInternalError, ClassInternal, "internal error"}
}

func tooLarge(limit int64) Failure {
	return Failure{http.StatusRequestEntityTooLarge, CodeRequestTooLarge, ClassInvalidRequest,
		"request body exceeds " + formatBytes(limit) + " limit"}
}

func formatBytes(n int64) string {
	if n >= 1_000_000 && n%1_000_000 == 0 {
		return fmt.Sprintf("%d MB", n/1_000_000)
	}
	return fmt.Sprintf("%d bytes", n)
}

// EndpointNotAllowed names only the static list of routes, so that the response
// never echoes a path chosen by the client.
func EndpointNotAllowed() Failure {
	return Failure{http.StatusNotFound, CodeEndpointNotAllowed, ClassInvalidRequest,
		"endpoint not allowed by Comporta; allowed endpoints: POST /v1/messages, " +
			"POST /v1/messages/count_tokens, POST /v1/chat/completions, GET /v1/models"}
}

// InvalidTarget rejects request targets that are not a plain path.
func InvalidTarget() Failure {
	return invalidRequest("request target must be a path; Comporta is not a forward proxy")
}

// HostNotAllowed names the host only when the caller vetted it as safe to echo.
func HostNotAllowed(safeHost string) Failure {
	message := "host not allowed; add it to allowed_hosts"
	if safeHost != "" {
		message = fmt.Sprintf("host %q not allowed; add it to allowed_hosts", safeHost)
	}
	return Failure{http.StatusMisdirectedRequest, CodeHostNotAllowed, ClassInvalidRequest, message}
}

func errorType(format auth.Format, status int) string {
	switch {
	case status == http.StatusUnauthorized:
		return "authentication_error"
	case status == http.StatusForbidden:
		return "permission_error"
	case status == http.StatusNotFound:
		return "not_found_error"
	case status == http.StatusRequestEntityTooLarge && format == auth.FormatAnthropic:
		return "request_too_large"
	case status == http.StatusTooManyRequests:
		return "rate_limit_error"
	case status == statusOverloaded:
		return "overloaded_error"
	case status >= 500 && format == auth.FormatAnthropic:
		return "api_error"
	case status >= 500:
		return "server_error"
	}
	return "invalid_request_error"
}

type errorBody struct {
	Type         string `json:"type"`
	Message      string `json:"message"`
	ComportaCode Code   `json:"comporta_code"`
	LimitName    string `json:"limit_name,omitempty"`
	Scope        string `json:"scope,omitempty"`
	Reason       string `json:"reason,omitempty"`
	RenewsAt     string `json:"renews_at,omitempty"`
}

func errorPayload(format auth.Format, failure Failure) []byte {
	return detailedPayload(format, failure, nil)
}

// detailedPayload adds optional limit fields. Errors without them keep the
// exact payload they always had.
func detailedPayload(format auth.Format, failure Failure, detail *LimitDetail) []byte {
	body := errorBody{Type: errorType(format, failure.Status), Message: failure.Message, ComportaCode: failure.Code}
	if detail != nil {
		body.LimitName, body.Scope, body.Reason = detail.LimitName, detail.Scope, detail.Reason
		if detail.RenewsAt != nil {
			body.RenewsAt = detail.RenewsAt.UTC().Format(time.RFC3339)
		}
	}
	var payload any = struct {
		Error errorBody `json:"error"`
	}{body}
	if format == auth.FormatAnthropic {
		payload = struct {
			Type  string    `json:"type"`
			Error errorBody `json:"error"`
		}{"error", body}
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return []byte(`{"error":{"type":"server_error","message":"internal error","comporta_code":"internal_error"}}`)
	}
	return data
}

// WriteFailure answers with the failure in the given format. It must only be used
// before the response is committed.
func WriteFailure(w http.ResponseWriter, format auth.Format, failure Failure) {
	writePayload(w, failure.Status, errorPayload(format, failure))
}

// writeRejection answers a refusal from a pipeline stage, with Retry-After and
// limit fields when the stage provided them.
func writeRejection(w http.ResponseWriter, format auth.Format, rejection *Rejection) {
	if rejection.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.FormatInt(rejection.RetryAfter, 10))
	}
	writePayload(w, rejection.Status, detailedPayload(format, rejection.failure(), rejection.Limit))
}

func writePayload(w http.ResponseWriter, status int, data []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

// sseErrorEvent renders the failure for a stream already committed, when the HTTP
// status can no longer change.
func sseErrorEvent(format auth.Format, failure Failure) []byte {
	data := errorPayload(format, failure)
	if format == auth.FormatAnthropic {
		return append(append([]byte("event: error\ndata: "), data...), "\n\n"...)
	}
	return append(append([]byte("data: "), data...), "\n\n"...)
}
