package proxy

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/auth"
	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/telemetry"
	"github.com/JoaoVictorVM/comporta/internal/translation"
)

const translatedHeader = "x-comporta-translated"

// TranslationSkip is one chain entry left out because it cannot represent the
// request. It holds closed values only: configured names, formats and a
// feature code.
type TranslationSkip struct {
	At       time.Time
	Position int
	Model    string
	Source   translation.WireFormat
	Target   translation.WireFormat
	Feature  translation.Feature
}

// TranslationRecorder makes a skip durable before the next candidate is
// considered. A skip is never an attempt: it reserves and consumes nothing.
type TranslationRecorder interface {
	RecordTranslationSkip(ctx context.Context, e *Envelope, skip TranslationSkip) error
}

func noEligibleModel(feature translation.Feature) *Rejection {
	return &Rejection{Status: http.StatusBadGateway, Code: CodeNoEligibleFallbackModel, Class: ClassProtocol,
		Message: "no eligible fallback model can represent this request (" + string(feature) + ")"}
}

func responseTranslationFailed() Failure {
	return Failure{http.StatusBadGateway, CodeResponseTranslationFailed, ClassProtocol, "response translation failed"}
}

// translatedValue names the direction of a converted answer, from the
// provider's protocol to the client's.
func translatedValue(client, target translation.WireFormat) string {
	switch {
	case client == translation.OpenAIResponses && target == translation.OpenAIChat:
		return "openai-chat->openai-responses"
	case target == translation.AnthropicMessages:
		return "anthropic->openai"
	}
	return "openai->anthropic"
}

func authFormat(format config.APIFormat) auth.Format {
	if format == config.FormatAnthropic {
		return auth.FormatAnthropic
	}
	return auth.FormatOpenAI
}

// needsTranslation reports whether a provider cannot take the client's body
// as it is. A Responses body always goes through the canonical form.
func (o *operation) needsTranslation(provider config.Provider) bool {
	return o.route.wire() != providerWire(provider.APIFormat)
}

// canonicalRequest builds the canonical form once per preparation, from the
// request already capped, pseudonymized and inspected. Every candidate is
// rendered from it, so none sees content the inspection did not.
func (o *operation) canonicalRequest(request *prepared) (*translation.Request, *Rejection) {
	if o.canonical == nil {
		stage := o.tel.Stage(telemetry.StageTranslateRequest)
		canonical, err := translation.ParseRequest(o.route.wire(), request.effectiveBody(), translation.ParseOptions{ProviderBeta: o.providerBeta})
		stage.End()
		var malformed *translation.MalformedError
		switch {
		case errors.As(err, &malformed):
			return nil, asRejection(invalidRequest(malformed.Message))
		case err != nil:
			return nil, asRejection(internalFailure())
		}
		o.canonical = canonical
	}
	return o.canonical, nil
}

// feasible reports the feature that keeps provider from representing the
// request, or an empty value.
func (o *operation) feasible(request *prepared, provider config.Provider) (translation.Feature, *Rejection) {
	if !o.needsTranslation(provider) {
		return "", nil
	}
	canonical, rejection := o.canonicalRequest(request)
	if rejection != nil {
		return "", rejection
	}
	return translation.Analyze(canonical, providerWire(provider.APIFormat)), nil
}

// render returns the body request sends to provider. A body in the provider's
// own protocol keeps the bytes and patches of the native path; any other is
// rendered from the canonical form, and its size replaces the effective size
// that quotes, reservations and limits read.
func (o *operation) render(request *prepared, provider config.Provider) ([]byte, bool, *Rejection) {
	if !o.needsTranslation(provider) {
		return request.effectiveBody(), false, nil
	}
	canonical, rejection := o.canonicalRequest(request)
	if rejection != nil {
		return nil, false, rejection
	}
	target := providerWire(provider.APIFormat)
	if feature := translation.Analyze(canonical, target); feature != "" {
		return nil, false, noEligibleModel(feature)
	}
	stage := o.tel.Stage(telemetry.StageTranslateRequest)
	body, err := translation.RenderRequest(canonical, target, request.envelope.ModelEffective)
	stage.End()
	if err != nil {
		o.tel.Translation(string(o.route.wire()), string(target), telemetry.TranslationFailed)
		return nil, false, asRejection(internalFailure())
	}
	if limit := o.snapshot.Listeners().MaxBodyBytes; int64(len(body)) > limit {
		return nil, false, asRejection(tooLarge(limit))
	}
	request.envelope.BodySizeEffective = int64(len(body))
	return body, true, nil
}

// chainPosition is the place of model in the app's fallback chain, from 1,
// or 0 for a requested model outside the chain.
func chainPosition(app config.App, model string) int {
	return slices.Index(app.Fallback, model) + 1
}

// skip makes the exclusion of model durable. Without accounting nothing is
// recorded; a failed write stops the operation, so no later candidate is sent
// without its diagnosis.
func (o *operation) skip(ctx context.Context, envelope *Envelope, app config.App, model string, provider config.Provider, feature translation.Feature) *Rejection {
	o.tel.TranslationSkipped(string(feature))
	recorder, ok := o.handler.accounting.(TranslationRecorder)
	if !ok {
		return nil
	}
	// The operation start keeps the event identical when a reload makes the
	// request prepare again, so the second write confirms the first.
	skip := TranslationSkip{At: o.admission, Position: chainPosition(app, model), Model: model, Source: o.route.wire(),
		Target: providerWire(provider.APIFormat), Feature: feature}
	if err := recorder.RecordTranslationSkip(ctx, envelope, skip); err != nil {
		if ctx.Err() != nil {
			return clientLeft()
		}
		return &Rejection{Status: http.StatusServiceUnavailable, Code: CodeUsageStorageUnavailable, Class: ClassInternal, Message: "usage storage unavailable"}
	}
	return nil
}

// substitute replaces a requested model that cannot represent the request with
// the first chain entry that can, before anything is quoted or reserved. Each
// model passed over is recorded as skipped. With none left the request ends
// with the feature of the requested model.
func (o *operation) substitute(ctx context.Context, plan *preparation, app config.App, feature translation.Feature) (*preparation, *Rejection) {
	envelope := plan.request.envelope
	if rejection := o.skip(ctx, envelope, app, envelope.ModelEffective, plan.provider, feature); rejection != nil {
		return nil, rejection
	}
	for _, model := range o.fallbackChain(app, envelope, envelope.ModelEffective) {
		name, provider, failure := o.resolveProvider(model)
		if failure != nil {
			continue
		}
		request, err := plan.request.withModel(model)
		if err != nil {
			return nil, asRejection(internalFailure())
		}
		blocked, rejection := o.feasible(request, provider)
		if rejection != nil {
			return nil, rejection
		}
		if blocked != "" {
			if rejection := o.skip(ctx, envelope, app, model, provider, blocked); rejection != nil {
				return nil, rejection
			}
			continue
		}
		body, translated, rejection := o.render(request, provider)
		if rejection != nil {
			return nil, rejection
		}
		return &preparation{request: request, effective: body, providerName: name, provider: provider, translated: translated}, nil
	}
	return nil, noEligibleModel(feature)
}

// nextCandidate returns the next chain entry that may be sent, recording the
// ones passed over because they cannot represent the request.
func (o *operation) nextCandidate(ctx context.Context, plan *preparation, app config.App, chain []string, sent, considered map[string]bool) (string, *Rejection) {
	for _, model := range chain {
		if sent[model] || considered[model] {
			continue
		}
		name, provider, failure := o.resolveProvider(model)
		if failure != nil || name == "" {
			considered[model] = true
			continue
		}
		feature, rejection := o.feasible(plan.request, provider)
		if rejection != nil {
			return "", rejection
		}
		if feature == "" {
			return model, nil
		}
		considered[model] = true
		if rejection := o.skip(ctx, plan.request.envelope, app, model, provider, feature); rejection != nil {
			return "", rejection
		}
	}
	return "", nil
}

// translatePlan prepares the requested model in its provider's protocol. A
// Responses body is validated here, after the inspection and before any
// selection. A model that cannot represent the request is replaced before
// admission by the first chain entry that can.
func (o *operation) translatePlan(ctx context.Context, plan *preparation, app config.App) (*preparation, *Rejection) {
	if plan.request.envelope.Operation != OperationGeneration {
		return plan, nil
	}
	if o.route == RouteResponses {
		if _, rejection := o.canonicalRequest(plan.request); rejection != nil {
			return nil, rejection
		}
	}
	feature, rejection := o.feasible(plan.request, plan.provider)
	if rejection != nil {
		return nil, rejection
	}
	if feature != "" {
		return o.substitute(ctx, plan, app, feature)
	}
	if !o.needsTranslation(plan.provider) {
		return plan, nil
	}
	body, translated, rejection := o.render(plan.request, plan.provider)
	if rejection != nil {
		return nil, rejection
	}
	plan.effective, plan.translated = body, translated
	return plan, nil
}

// toTranslationUsage hands the provider's normalized counters to the
// renderer; the settlement keeps reading the originals.
func toTranslationUsage(u Usage) translation.Usage {
	counter := func(v Optional[int64]) translation.Counter {
		return translation.Counter{Value: v.Value, Known: v.Known}
	}
	return translation.Usage{Input: counter(u.Input), Output: counter(u.Output), CacheRead: counter(u.CacheRead), CacheWrite: counter(u.CacheWrite)}
}
