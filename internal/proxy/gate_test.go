package proxy

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/runtimecontrol"
	"github.com/JoaoVictorVM/comporta/internal/storage"
)

// fakeGate refuses with a fixed error and counts open tickets through a real
// control, so a missing or doubled release shows up as a wrong count.
type fakeGate struct {
	mu      sync.Mutex
	refuse  error
	control *runtimecontrol.Control
	admits  int
}

func (g *fakeGate) Admit(subject runtimecontrol.Subject) (*runtimecontrol.Ticket, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.refuse != nil {
		return nil, g.refuse
	}
	g.admits++
	return g.control.Admit(subject)
}

type emptyPauses struct{}

func (emptyPauses) LoadPause(context.Context) (storage.PauseState, error) {
	return storage.PauseState{}, nil
}

func (emptyPauses) SavePause(context.Context, storage.PauseChange) (storage.PauseState, bool, error) {
	return storage.PauseState{}, false, nil
}

func TestPausedAppGetsSanitized503(t *testing.T) {
	upstream, server := startUpstream(t, nil)
	control := runtimecontrol.New(emptyPauses{})
	if err := control.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	gate := &fakeGate{refuse: runtimecontrol.ErrPaused, control: control}
	fx := newFixture(t, server.URL, fixtureOptions{gate: gate})
	for _, c := range []struct{ path, format, body string }{
		{"/v1/chat/completions", "openai", openAIBody},
		{"/v1/messages", "anthropic", anthropicBody},
		{"/v1/messages/count_tokens", "anthropic", countBody},
	} {
		response, body := fx.post(c.path, c.format, c.body)
		if response.StatusCode != http.StatusServiceUnavailable || comportaCode(t, body) != "app_paused" || !strings.Contains(body, "app bot is paused by an operator") {
			t.Fatalf("%s: %d %s", c.path, response.StatusCode, body)
		}
		if response.Header.Get("Retry-After") != "" {
			t.Fatal("a pause has no known end")
		}
	}
	if upstream.count() != 0 {
		t.Fatal("a paused request reached the provider")
	}
	response, _ := fx.do(fx.request(http.MethodGet, "/v1/models", "openai", ""))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("model listing is local and stays available: %d", response.StatusCode)
	}

	gate.refuse = runtimecontrol.ErrMaintenance
	response, body := fx.post("/v1/chat/completions", "openai", openAIBody)
	if response.StatusCode != http.StatusServiceUnavailable || comportaCode(t, body) != "user_maintenance" {
		t.Fatalf("maintenance: %d %s", response.StatusCode, body)
	}
	gate.refuse = runtimecontrol.ErrUnavailable
	if response, _ := fx.post("/v1/chat/completions", "openai", openAIBody); response.StatusCode != http.StatusInternalServerError {
		t.Fatalf("an unavailable control must fail closed: %d", response.StatusCode)
	}
	gate.refuse = nil
	if response, _ := fx.post("/v1/chat/completions", "openai", openAIBody); response.StatusCode != http.StatusOK || gate.admits != 1 {
		t.Fatalf("an open gate: %d", response.StatusCode)
	}
	fx.sink.wait(t)
	deadline := time.Now().Add(5 * time.Second)
	for control.InFlight() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the ticket was not released when the request ended")
		}
		time.Sleep(time.Millisecond)
	}
}
