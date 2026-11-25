package translation

// anthropicMediaTypes are the inline image types the Messages API accepts.
var anthropicMediaTypes = map[string]bool{"image/jpeg": true, "image/png": true, "image/gif": true, "image/webp": true}

// Analyze reports the first feature of the request that target cannot
// represent, or an empty value when the translation keeps every semantic.
// The request's own protocol always represents it.
func Analyze(r *Request, target WireFormat) Feature {
	if target == r.Source {
		return ""
	}
	if r.blocked != "" {
		return r.blocked
	}
	if target == OpenAIResponses {
		return FeatureResponseShape
	}
	names := map[string]bool{}
	for _, tool := range r.Tools {
		names[tool.Name] = true
	}
	switch r.ToolChoice.Mode {
	case ChoiceRequired:
		if len(r.Tools) == 0 {
			return FeatureToolChoice
		}
	case ChoiceNamed:
		if !names[r.ToolChoice.Name] {
			return FeatureToolChoice
		}
	}
	for _, turn := range r.Turns {
		for _, block := range turn.Blocks {
			if feature := analyzeBlock(block, turn.Role, target); feature != "" {
				return feature
			}
		}
	}
	return ""
}

func analyzeBlock(block Block, role Role, target WireFormat) Feature {
	switch block.Kind {
	case BlockImage:
		if role != RoleUser {
			return FeatureUnsupportedBlock
		}
		return analyzeImage(block.Image, target)
	case BlockToolCall:
		if role != RoleAssistant {
			return FeatureUnsupportedRole
		}
	case BlockToolResult:
		if role != RoleUser {
			return FeatureUnsupportedRole
		}
		if target == OpenAIChat && block.IsError {
			return FeatureToolResult
		}
		for _, part := range block.Result {
			if part.Kind == BlockImage && target == OpenAIChat {
				return FeatureToolResult
			}
			if part.Kind == BlockImage {
				if feature := analyzeImage(part.Image, target); feature != "" {
					return feature
				}
			}
		}
	case BlockText:
	default:
		return FeatureUnsupportedBlock
	}
	return ""
}

func analyzeImage(image Image, target WireFormat) Feature {
	if target == AnthropicMessages && image.inline() && !anthropicMediaTypes[image.MediaType] {
		return FeatureImageSource
	}
	return ""
}
