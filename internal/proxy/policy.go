package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/JoaoVictorVM/comporta/internal/config"
)

const downgradedFromHeader = "x-comporta-downgraded-from"

// preparation is everything that depends on the snapshot: the request as it
// would be sent, its destination and, for a downgrade policy, the target.
type preparation struct {
	request      *prepared
	effective    []byte
	providerName string
	provider     config.Provider
	target       *target
	// translated is set when effective was rendered for another protocol.
	translated bool
}

// target is the downgrade candidate, built from the request after identity,
// output cap and sensitive inspection, so it carries exactly the same content.
type target struct {
	model        string
	request      *prepared
	effective    []byte
	providerName string
	provider     config.Provider
	unavailable  *Rejection
	translated   bool
}

func modelNotAllowed(model, app string) *Rejection {
	return &Rejection{Status: http.StatusForbidden, Code: CodeModelNotAllowed, Class: ClassInvalidRequest,
		Message: fmt.Sprintf("model %q is not allowed for app %s", boundedText(model), app)}
}

func boundedText(s string) string {
	if len(s) > 120 {
		return s[:120]
	}
	return s
}

func asRejection(f Failure) *Rejection {
	return &Rejection{Status: f.Status, Code: f.Code, Class: f.Class, Message: f.Message}
}

// withModel returns a copy of the request whose body names another model. Only
// the model value changes; the hashes and sizes follow the new body.
func (p *prepared) withModel(model string) (*prepared, error) {
	span, ok := p.document.Path("model")
	if !ok {
		return nil, errors.New("model field missing")
	}
	text, err := json.Marshal(model)
	if err != nil {
		return nil, err
	}
	patch := Patch{At: span, Text: text}
	if overlaps(p.patches, []Patch{patch}) {
		return nil, errors.New("model field already patched")
	}
	envelope := *p.envelope
	envelope.ModelEffective = model
	out := &prepared{envelope: &envelope, document: p.document, patches: append(slices.Clone(p.patches), patch), uncacheable: p.uncacheable}
	out.refreshHashes()
	return out, nil
}

// downgradeTarget prepares the target of an app with on_limit downgrade. A
// request that already names the target has nothing to switch to. A target
// that cannot be sent from this endpoint is still described, so the policy
// refuses with that reason instead of sending the original past its budget.
func (o *operation) downgradeTarget(request *prepared, app config.App) *target {
	e := request.envelope
	if e.Operation != OperationGeneration || app.OnLimit != config.OnLimitDowngrade || app.DowngradeTo == "" || app.DowngradeTo == e.ModelRequested {
		return nil
	}
	return o.buildTarget(request, app)
}

// policyTarget is the downgrade target of a later attempt on model. A target
// that already failed in this request is described as unavailable: the policy
// then refuses instead of sending it again or letting a costlier model through.
func (o *operation) policyTarget(request *prepared, app config.App, model string, sent map[string]bool) *target {
	if app.OnLimit != config.OnLimitDowngrade || app.DowngradeTo == "" || app.DowngradeTo == model {
		return nil
	}
	if sent[app.DowngradeTo] {
		return &target{model: app.DowngradeTo, unavailable: fallbackTargetExhausted(app.DowngradeTo)}
	}
	return o.buildTarget(request, app)
}

func (o *operation) buildTarget(request *prepared, app config.App) *target {
	out := &target{model: app.DowngradeTo}
	name, declared := o.snapshot.ProviderForModel(app.DowngradeTo)
	provider, found := o.snapshot.Provider(name)
	switch {
	case !declared || !found:
		out.unavailable = asRejection(internalFailure())
		return out
	case provider.APIFormat != providerFormat(o.format):
		out.unavailable = &Rejection{Status: http.StatusBadRequest, Code: CodeProviderFormatMismatch, Class: ClassInvalidRequest,
			Message: "the downgrade target uses a different API format than this endpoint; the request was not sent"}
		return out
	}
	prepared, err := request.withModel(app.DowngradeTo)
	if err != nil {
		out.unavailable = asRejection(internalFailure())
		return out
	}
	body, translated, rejection := o.render(prepared, provider)
	if rejection != nil {
		out.unavailable = rejection
		return out
	}
	out.request, out.effective, out.providerName, out.provider, out.translated = prepared, body, name, provider, translated
	return out
}

func (p *preparation) candidates() []Candidate {
	e := p.request.envelope
	out := []Candidate{{ID: CandidateOriginal, Model: e.ModelEffective, ProviderName: p.providerName, Provider: p.provider, BodySizeEffective: e.BodySizeEffective}}
	if t := p.target; t != nil {
		c := Candidate{ID: CandidateDowngrade, Model: t.model, ProviderName: t.providerName, Provider: t.provider, Unavailable: t.unavailable}
		if t.request != nil {
			c.BodySizeEffective = t.request.envelope.BodySizeEffective
		}
		out = append(out, c)
	}
	return out
}

// chosen is what the operation sends after admission.
type chosen struct {
	request      *prepared
	envelope     *Envelope
	body         []byte
	providerName string
	provider     config.Provider
	downgraded   bool
	translated   bool
}

// selected returns the only candidate the admission authorized. Anything else
// than a prepared target falls back to the original, which was what was priced
// as the original.
func (p *preparation) selected(a Admission) (chosen, bool) {
	if a.Selected != CandidateDowngrade {
		return chosen{request: p.request, envelope: p.request.envelope, body: p.effective, providerName: p.providerName, provider: p.provider, translated: p.translated}, true
	}
	t := p.target
	if t == nil || t.request == nil {
		return chosen{}, false
	}
	return chosen{request: t.request, envelope: t.request.envelope, body: t.effective, providerName: t.providerName, provider: t.provider, downgraded: true, translated: t.translated}, true
}
