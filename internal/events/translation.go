package events

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// TranslationSkipped records a model passed over because it cannot represent
// the request in its protocol. It is never an attempt.
const TranslationSkipped Type = "translation_skipped"

// ReasonUntranslatable is the only reason of a translation skip.
const ReasonUntranslatable = "untranslatable_feature"

// maxChainPosition bounds the position well above any configured chain.
const maxChainPosition = 64

var translationFormats = map[string]bool{"anthropic_messages": true, "openai_chat": true, "openai_responses": true}

var translationFeatures = map[string]bool{"provider_beta": true, "unknown_field": true, "unsupported_role": true, "unsupported_content": true,
	"image_source": true, "tool_type": true, "tool_choice": true, "tool_result": true, "structured_output": true, "reasoning": true,
	"stateful_response": true, "audio": true, "file": true, "response_shape": true}

// TranslationSkipID derives the event of one skip from the request, the chain
// position, the model and the feature, so a retried write is the same event.
func TranslationSkipID(requestID string, position int, model, feature string) string {
	id := sha256.Sum256([]byte(fmt.Sprintf("translation_skipped:%s:%d:%d:%s:%s", requestID, position, len(model), model, feature)))
	return hex.EncodeToString(id[:16])
}

// NewTranslationSkip builds the event of one skipped model. It carries a
// configured model name, closed formats and a closed feature: never content,
// a user or an attempt.
func NewTranslationSkip(at time.Time, requestID, app string, position int, model, source, target, feature string) Event {
	return Event{
		ID: TranslationSkipID(requestID, position, model, feature), Type: TranslationSkipped, At: at.UTC(), RequestID: requestID, App: app,
		Payload: Payload{ChainPosition: position, Model: model, SourceFormat: source, TargetFormat: target, Reason: ReasonUntranslatable, Feature: feature},
	}
}

// validateTranslationFields keeps the skip fields on skip events only, with
// closed values.
func (e Event) validateTranslationFields() error {
	p := e.Payload
	has := p.ChainPosition != 0 || p.Model != "" || p.SourceFormat != "" || p.TargetFormat != "" || p.Feature != ""
	if e.Type != TranslationSkipped {
		if has || p.Reason == ReasonUntranslatable {
			return errors.New("invalid event payload")
		}
		return nil
	}
	if p.ChainPosition < 0 || p.ChainPosition > maxChainPosition || (p.Model != "" && !modelName.MatchString(p.Model)) ||
		(p.SourceFormat != "" && !translationFormats[p.SourceFormat]) || (p.TargetFormat != "" && !translationFormats[p.TargetFormat]) ||
		(p.Feature != "" && !translationFeatures[p.Feature]) || (p.Reason != "" && p.Reason != ReasonUntranslatable) {
		return errors.New("invalid translation skip payload")
	}
	return nil
}

// ValidateTranslationSkip holds a new skip event to its full contract.
func (e Event) ValidateTranslationSkip() error {
	if err := e.Validate(); err != nil {
		return err
	}
	p := e.Payload
	rest := p
	rest.ChainPosition, rest.Model, rest.SourceFormat, rest.TargetFormat, rest.Reason, rest.Feature = 0, "", "", "", "", ""
	if e.Type != TranslationSkipped || e.RequestID == "" || e.App == "" || p.Model == "" || p.SourceFormat == "" || p.TargetFormat == "" ||
		p.SourceFormat == p.TargetFormat || p.Feature == "" || p.Reason != ReasonUntranslatable ||
		e.ID != TranslationSkipID(e.RequestID, p.ChainPosition, p.Model, p.Feature) || !isZeroPayload(rest) {
		return errors.New("invalid translation skip event")
	}
	return nil
}

func isZeroPayload(p Payload) bool {
	return p.ErrorClass == "" && p.ErrorCode == "" && p.Outcome == "" && p.AttemptIndex == 0 && p.Cost == 0 && p.Count == 0 && !p.UsageMissing &&
		p.Paused == nil && p.Generation == 0 && p.Result == "" && len(p.Changes) == 0 && p.Severity == "" && p.LimitName == "" && p.Scope == "" &&
		p.DegradedSeconds == 0 && p.WindowMS == 0 && p.CooldownMS == 0 && p.Detections == nil && p.ModelRequested == "" && p.ModelUsed == "" &&
		p.FromModel == "" && p.ToModel == "" && p.Cause == ""
}
