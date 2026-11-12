// Package fakeprovider imitates the Anthropic and OpenAI APIs with synthetic,
// deterministic responses, so that tests and the demo need no keys and no network.
package fakeprovider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"
)

// maxRequestBytes keeps a misuse of the fake from filling memory.
const maxRequestBytes = 64 << 20

// Request is what the fake received, for tests that compare forwarded bytes.
type Request struct {
	Method string
	Path   string
	Header http.Header
	Body   []byte
}

// Options configures the handler. Everything is optional.
type Options struct {
	Scenario Scenario
	// SlowDelay is the wait between events of the slow-stream scenario.
	SlowDelay time.Duration
	// Sleep replaces the wait, so that tests can hold or release events.
	Sleep func(ctx context.Context, delay time.Duration) error
	// OnRequest observes each request. Nothing is stored otherwise.
	OnRequest func(Request)
	// OnCancel is called when a client abandons a request that was still running.
	OnCancel func()
}

// New returns the handler.
func New(options Options) http.Handler {
	if options.Scenario == "" {
		options.Scenario = ScenarioSuccess
	}
	if options.SlowDelay <= 0 {
		options.SlowDelay = defaultSlowDelay
	}
	if options.Sleep == nil {
		options.Sleep = sleepContext
	}
	return &handler{options: options}
}

type handler struct {
	options Options
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	if err != nil {
		writeError(w, false, http.StatusRequestEntityTooLarge, "invalid_request_error", "request body could not be read")
		return
	}
	if h.options.OnRequest != nil {
		h.options.OnRequest(Request{Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone(), Body: body})
	}
	route := r.Method + " " + r.URL.Path
	anthropic := route != "POST /v1/chat/completions"
	switch route {
	case "POST /v1/messages", "POST /v1/chat/completions":
		h.serveConversation(w, r, body, anthropic)
	case "POST /v1/messages/count_tokens":
		if h.failByScenario(w, r, true) {
			return
		}
		writeJSON(w, `{"input_tokens":10}`)
	case "GET /v1/models":
		writeModels(w, r)
	default:
		writeError(w, true, http.StatusNotFound, "not_found_error", "unknown fake route")
	}
}

type conversationRequest struct {
	Model         string `json:"model"`
	Stream        bool   `json:"stream"`
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
}

func (h *handler) serveConversation(w http.ResponseWriter, r *http.Request, body []byte, anthropic bool) {
	var parsed conversationRequest
	if err := json.Unmarshal(body, &parsed); err != nil {
		writeError(w, anthropic, http.StatusBadRequest, "invalid_request_error", "invalid JSON")
		return
	}
	if h.failByScenario(w, r, anthropic) {
		return
	}
	loop := h.options.Scenario == ScenarioAgentLoop
	includeUsage := parsed.StreamOptions != nil && parsed.StreamOptions.IncludeUsage
	switch {
	case !parsed.Stream && anthropic && loop:
		writeJSON(w, anthropicToolUse(parsed.Model))
	case !parsed.Stream && loop:
		writeJSON(w, openAIToolCall(parsed.Model))
	case !parsed.Stream && anthropic:
		writeJSON(w, anthropicMessage(parsed.Model))
	case !parsed.Stream:
		writeJSON(w, openAICompletion(parsed.Model))
	case anthropic && loop:
		h.stream(w, r, anthropicToolEvents(parsed.Model))
	case loop:
		h.stream(w, r, openAIToolEvents(parsed.Model, includeUsage))
	case anthropic:
		h.stream(w, r, anthropicEvents(parsed.Model))
	default:
		h.stream(w, r, openAIEvents(parsed.Model, includeUsage))
	}
}

// failByScenario answers the scenarios that end before any success byte.
func (h *handler) failByScenario(w http.ResponseWriter, r *http.Request, anthropic bool) bool {
	switch h.options.Scenario {
	case ScenarioRateLimit:
		w.Header().Set("Retry-After", "1")
		writeError(w, anthropic, http.StatusTooManyRequests, "rate_limit_error", "fake rate limit")
		return true
	case ScenarioOverload:
		writeError(w, anthropic, 529, "overloaded_error", "fake overload")
		return true
	case ScenarioHeaderTimeout:
		<-r.Context().Done()
		h.notifyCancel()
		return true
	}
	return false
}

func (h *handler) stream(w http.ResponseWriter, r *http.Request, events []string) {
	w.Header().Set("Content-Type", "text/event-stream")
	controller := http.NewResponseController(w)
	for i, event := range events {
		if h.options.Scenario == ScenarioDisconnectMidstream && i == len(events)/2 {
			// Closing the connection without finishing the chunked body is what a
			// provider failing mid-stream looks like on the wire.
			panic(http.ErrAbortHandler)
		}
		if _, err := io.WriteString(w, event); err != nil {
			h.notifyCancel()
			return
		}
		if err := controller.Flush(); err != nil {
			h.notifyCancel()
			return
		}
		if h.options.Scenario == ScenarioSlowStream && i < len(events)-1 {
			if err := h.options.Sleep(r.Context(), h.options.SlowDelay); err != nil {
				h.notifyCancel()
				return
			}
		}
		if r.Context().Err() != nil {
			h.notifyCancel()
			return
		}
	}
}

func (h *handler) notifyCancel() {
	if h.options.OnCancel != nil {
		h.options.OnCancel()
	}
}

func writeJSON(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, body)
}

func writeModels(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Api-Key") != "" {
		writeJSON(w, `{"data":[{"id":"fake-claude","type":"model","display_name":"fake-claude","created_at":"1970-01-01T00:00:00Z"}],"has_more":false,"first_id":"fake-claude","last_id":"fake-claude"}`)
		return
	}
	writeJSON(w, `{"object":"list","data":[{"id":"fake-gpt","object":"model","created":0,"owned_by":"fake"}]}`)
}

func writeError(w http.ResponseWriter, anthropic bool, status int, kind, message string) {
	type detail struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	}
	var payload any = struct {
		Error detail `json:"error"`
	}{detail{kind, message}}
	if anthropic {
		payload = struct {
			Type  string `json:"type"`
			Error detail `json:"error"`
		}{"error", detail{kind, message}}
	}
	data, err := json.Marshal(payload)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}
