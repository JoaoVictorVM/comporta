package proxy

import (
	"net/http"

	"github.com/JoaoVictorVM/comporta/internal/auth"
	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/translation"
)

// Route is one of the fixed endpoints the gateway serves.
type Route int

const (
	RouteMessages Route = iota + 1
	RouteCountTokens
	RouteChatCompletions
	RouteModels
	RouteResponses
)

// MatchRoute compares the method and the raw path by equality. Encoded variants,
// repeated slashes and dot segments never match, so no normalization can turn a
// forbidden path into an allowed one.
func MatchRoute(method, path string) (Route, bool) {
	switch {
	case method == http.MethodPost && path == "/v1/messages":
		return RouteMessages, true
	case method == http.MethodPost && path == "/v1/messages/count_tokens":
		return RouteCountTokens, true
	case method == http.MethodPost && path == "/v1/chat/completions":
		return RouteChatCompletions, true
	case method == http.MethodGet && path == "/v1/models":
		return RouteModels, true
	case method == http.MethodPost && path == "/v1/responses":
		return RouteResponses, true
	}
	return 0, false
}

func (r Route) format(request *http.Request) auth.Format {
	switch r {
	case RouteMessages, RouteCountTokens:
		return auth.FormatAnthropic
	case RouteChatCompletions, RouteResponses:
		return auth.FormatOpenAI
	}
	return auth.ModelsFormat(request)
}

func (r Route) path() string {
	switch r {
	case RouteMessages:
		return "/v1/messages"
	case RouteCountTokens:
		return "/v1/messages/count_tokens"
	case RouteChatCompletions:
		return "/v1/chat/completions"
	case RouteResponses:
		return "/v1/responses"
	}
	return "/v1/models"
}

// wire is the conversational protocol of the route's body. The Responses API
// authenticates as OpenAI but its body is not a Chat Completions body.
func (r Route) wire() translation.WireFormat {
	switch r {
	case RouteMessages, RouteCountTokens:
		return translation.AnthropicMessages
	case RouteResponses:
		return translation.OpenAIResponses
	}
	return translation.OpenAIChat
}

// providerWire is the protocol a provider speaks.
func providerWire(format config.APIFormat) translation.WireFormat {
	if format == config.FormatAnthropic {
		return translation.AnthropicMessages
	}
	return translation.OpenAIChat
}

func providerFormat(format auth.Format) config.APIFormat {
	if format == auth.FormatAnthropic {
		return config.FormatAnthropic
	}
	return config.FormatOpenAI
}
