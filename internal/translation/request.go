package translation

import (
	"errors"
)

// ParseOptions carries what the proxy knows about a request beyond its body.
type ParseOptions struct {
	// ProviderBeta is set when the client asked for an Anthropic beta, which
	// has no equivalent in another protocol.
	ProviderBeta bool
}

// ParseRequest builds the canonical request from the effective body of a
// request. Anthropic Messages and Chat Completions bodies are already accepted
// by the proxy and keep their native path, so anything the representation
// cannot hold only blocks translation. A Responses body is always translated:
// an invalid one is a MalformedError.
func ParseRequest(format WireFormat, body []byte, options ParseOptions) (*Request, error) {
	root, ok := decodeObject(body)
	if !ok {
		return nil, malformed("request body must be a JSON object")
	}
	var request *Request
	var err error
	switch format {
	case AnthropicMessages:
		request = parseAnthropicRequest(root)
	case OpenAIChat:
		request = parseChatRequest(root)
	case OpenAIResponses:
		request, err = parseResponsesRequest(root)
	default:
		return nil, errors.New("unknown wire format")
	}
	if err != nil {
		return nil, err
	}
	if options.ProviderBeta {
		request.blocked = FeatureProviderBeta
	}
	return request, nil
}

// RenderRequest writes the request in target for model. It never changes the
// request, so every candidate of an operation renders from the same content.
// A request that Analyze did not accept must not be rendered.
func RenderRequest(r *Request, target WireFormat, model string) ([]byte, error) {
	if feature := Analyze(r, target); feature != "" && target != r.Source {
		return nil, errors.New("request cannot be represented in the target format")
	}
	switch target {
	case AnthropicMessages:
		return renderAnthropicRequest(r, model)
	case OpenAIChat:
		return renderChatRequest(r, model)
	}
	return nil, errors.New("requests are not rendered in this format")
}

// UpstreamPath is the provider endpoint of a protocol.
func UpstreamPath(target WireFormat) string {
	if target == AnthropicMessages {
		return "/v1/messages"
	}
	return "/v1/chat/completions"
}

// splitDataURL reads a base64 data URL. Anything else is a remote reference.
func splitDataURL(url string) (Image, bool) {
	const prefix = "data:"
	if len(url) < len(prefix) || url[:len(prefix)] != prefix {
		return Image{URL: url}, true
	}
	rest := url[len(prefix):]
	for i := 0; i < len(rest); i++ {
		if rest[i] == ',' {
			meta, data := rest[:i], rest[i+1:]
			const marker = ";base64"
			if len(meta) <= len(marker) || meta[len(meta)-len(marker):] != marker || data == "" {
				return Image{}, false
			}
			return Image{MediaType: meta[:len(meta)-len(marker)], Data: data}, true
		}
	}
	return Image{}, false
}

// appendTurn adds blocks to the conversation, joining them to the previous
// turn of the same role when join is set, which is how tool results of one
// assistant turn travel together.
func appendTurn(turns []Turn, role Role, blocks []Block, join bool) []Turn {
	if join && len(turns) > 0 && turns[len(turns)-1].Role == role {
		last := &turns[len(turns)-1]
		last.Blocks = append(last.Blocks, blocks...)
		return turns
	}
	return append(turns, Turn{Role: role, Blocks: blocks})
}

func onlyToolResults(turn Turn) bool {
	for _, block := range turn.Blocks {
		if block.Kind != BlockToolResult {
			return false
		}
	}
	return len(turn.Blocks) > 0
}
