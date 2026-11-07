package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/JoaoVictorVM/comporta/internal/auth"
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
}

func errorPayload(format auth.Format, failure Failure) []byte {
	body := errorBody{Type: errorType(format, failure.Status), Message: failure.Message, ComportaCode: failure.Code}
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
	data := errorPayload(format, failure)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(failure.Status)
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
