package proxy

import (
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strconv"

	"github.com/JoaoVictorVM/comporta/internal/auth"
	"github.com/JoaoVictorVM/comporta/internal/config"
)

const (
	modelsDefaultLimit = 100
	modelsMaxLimit     = 1000
	// The configuration holds no release dates, so both formats carry a fixed value.
	modelsCreatedAt = "1970-01-01T00:00:00Z"
	modelsOwnedBy   = "comporta"
)

// catalog lists the models of the app in the API format of the credential. It is
// computed locally: listing costs nothing and tells nothing about provider health.
func catalog(snapshot *config.Snapshot, appName string, format auth.Format) []string {
	app, ok := snapshot.App(appName)
	if !ok {
		return nil
	}
	want := providerFormat(format)
	models := make([]string, 0, len(app.AllowedModels))
	for _, model := range app.AllowedModels {
		owner, declared := snapshot.ProviderForModel(model)
		if !declared {
			continue
		}
		if provider, exists := snapshot.Provider(owner); exists && provider.APIFormat == want {
			models = append(models, model)
		}
	}
	slices.Sort(models)
	return slices.Compact(models)
}

func serveModels(w http.ResponseWriter, r *http.Request, format auth.Format, snapshot *config.Snapshot, appName, rawQuery string) {
	models := catalog(snapshot, appName, format)
	var payload any
	var failure *Failure
	if format == auth.FormatOpenAI {
		if rawQuery != "" {
			rejected := invalidRequest("this endpoint accepts no query parameters")
			failure = &rejected
		} else {
			payload = openAIModels(models)
		}
	} else {
		payload, failure = anthropicModels(models, rawQuery)
	}
	if failure != nil {
		WriteFailure(w, format, *failure)
		return
	}
	data, err := json.Marshal(payload)
	if err != nil {
		WriteFailure(w, format, internalFailure())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}

type openAIModel struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

func openAIModels(models []string) any {
	data := make([]openAIModel, 0, len(models))
	for _, model := range models {
		data = append(data, openAIModel{ID: model, Object: "model", OwnedBy: modelsOwnedBy})
	}
	return struct {
		Object string        `json:"object"`
		Data   []openAIModel `json:"data"`
	}{"list", data}
}

type anthropicModel struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	DisplayName string `json:"display_name"`
	CreatedAt   string `json:"created_at"`
}

type anthropicPage struct {
	Data    []anthropicModel `json:"data"`
	HasMore bool             `json:"has_more"`
	FirstID *string          `json:"first_id"`
	LastID  *string          `json:"last_id"`
}

// anthropicModels pages by model ID. before_id returns the entries closest to the
// cursor, as the provider API does.
func anthropicModels(models []string, rawQuery string) (any, *Failure) {
	reject := func(message string) (any, *Failure) {
		failure := invalidRequest(message)
		return nil, &failure
	}
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		return reject("invalid query string")
	}
	limit := modelsDefaultLimit
	for name, list := range values {
		if len(list) != 1 {
			return reject("query parameters must not be repeated")
		}
		switch name {
		case "limit":
			parsed, parseErr := strconv.Atoi(list[0])
			if parseErr != nil || parsed < 1 || parsed > modelsMaxLimit {
				return reject("limit must be an integer from 1 to 1000")
			}
			limit = parsed
		case "after_id", "before_id":
		default:
			return reject("unknown query parameter")
		}
	}
	_, hasAfter := values["after_id"]
	_, hasBefore := values["before_id"]
	if hasAfter && hasBefore {
		return reject("use after_id or before_id, not both")
	}

	start, end, hasMore := 0, len(models), false
	switch {
	case hasAfter:
		index := slices.Index(models, values.Get("after_id"))
		if index < 0 {
			return reject("after_id is not in the catalog")
		}
		start = index + 1
	case hasBefore:
		index := slices.Index(models, values.Get("before_id"))
		if index < 0 {
			return reject("before_id is not in the catalog")
		}
		end = index
	}
	if end-start > limit {
		hasMore = true
		if hasBefore {
			start = end - limit
		} else {
			end = start + limit
		}
	}

	page := anthropicPage{Data: make([]anthropicModel, 0, end-start), HasMore: hasMore}
	for _, model := range models[start:end] {
		page.Data = append(page.Data, anthropicModel{ID: model, Type: "model", DisplayName: model, CreatedAt: modelsCreatedAt})
	}
	if len(page.Data) > 0 {
		first, last := page.Data[0].ID, page.Data[len(page.Data)-1].ID
		page.FirstID, page.LastID = &first, &last
	}
	return page, nil
}
