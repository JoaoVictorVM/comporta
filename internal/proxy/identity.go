package proxy

import (
	"errors"
	"net/http"

	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/useridentity"
)

const metadataMustBeObject = "metadata must be an object when setting user identity"

// applyIdentity is a stage of its own, before Prepare, and not a hook: a hook has no
// way to add patches, and the effective body comes from the patches alone. On success
// the raw identifier is gone from the envelope and no identity field of the outgoing
// body holds anything but the pseudonym. On failure nothing was added, so a partially
// pseudonymized body can never be forwarded.
func (p *prepared) applyIdentity(service *useridentity.Service, route Route, snapshot *config.Snapshot) *Failure {
	if service == nil {
		failure := internalFailure()
		return &failure
	}
	envelope := p.envelope
	app, ok := snapshot.App(envelope.AppName)
	if !ok {
		failure := internalFailure()
		return &failure
	}
	raw, _, present := envelope.RawUser()
	identity, err := service.Resolve(envelope.AppName, raw, present, app.RequireUser)
	if err != nil {
		failure := identityFailure(err)
		return &failure
	}
	patches, failure := identityPatches(p.document, route, identity.Hash().Hex())
	if failure != nil {
		return failure
	}
	p.patches = append(p.patches, patches...)
	envelope.Identity = identity
	envelope.rawUser = ""
	p.refreshHashes()
	return nil
}

func identityFailure(err error) Failure {
	code := CodeUserInvalid
	switch {
	case errors.Is(err, useridentity.ErrUserRequired):
		code = CodeUserRequired
	case errors.Is(err, useridentity.ErrUserEmail):
		code = CodeUserEmailNotAllowed
	case errors.Is(err, useridentity.ErrUserDocument):
		code = CodeUserDocumentNotAllowed
	case !errors.Is(err, useridentity.ErrUserInvalid):
		return internalFailure()
	}
	return Failure{http.StatusBadRequest, code, ClassInvalidRequest, err.Error()}
}

// identityPatches replaces every known identity field that is present, whichever one
// won the precedence, and adds the provider's native field when it is missing. The
// advisory count adds nothing: it consumes no tokens and needs no pseudonym.
func identityPatches(document *Document, route Route, hash string) ([]Patch, *Failure) {
	value := []byte(`"` + hash + `"`)
	var patches []Patch
	metadataUser, hasMetadataUser := document.Path("metadata", "user_id")
	if hasMetadataUser {
		patches = append(patches, Patch{At: metadataUser, Text: value})
	}
	user, hasUser := document.Path("user")
	if hasUser {
		patches = append(patches, Patch{At: user, Text: value})
	}

	var native Patch
	var err error
	switch {
	case route == RouteMessages && !hasMetadataUser:
		metadata, hasMetadata := document.Path("metadata")
		switch {
		case !hasMetadata:
			native, err = document.SetMember(document.Root(), "metadata", []byte(`{"user_id":`+string(value)+`}`))
		case document.Kind(metadata) != '{':
			failure := invalidRequest(metadataMustBeObject)
			return nil, &failure
		default:
			native, err = document.SetMember(metadata, "user_id", value)
		}
	case route == RouteChatCompletions && !hasUser:
		native, err = document.SetMember(document.Root(), "user", value)
	default:
		return patches, nil
	}
	if err != nil {
		failure := internalFailure()
		return nil, &failure
	}
	return append(patches, native), nil
}
