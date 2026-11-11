package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/runtimecontrol"
	"github.com/JoaoVictorVM/comporta/internal/storage"
	"github.com/JoaoVictorVM/comporta/internal/storage/sqlite"
	"github.com/JoaoVictorVM/comporta/internal/useridentity"
)

const serviceConfig = `timezone: UTC
providers:
  openai:
    api_format: openai
    base_url: http://127.0.0.1:9/v1
    models: [fake-gpt]
apps:
  bot-discord:
    allowed_models: [fake-gpt]
    timezone: America/Sao_Paulo
  other:
    allowed_models: [fake-gpt]
`

type noLimits struct{}

func (noLimits) ForgetSubjects(_ []storage.SubjectRef, persist func() error) (int, error) {
	return 0, persist()
}
func (noLimits) Apps() []string { return nil }

type serviceFixture struct {
	*apiFixture
	service *Service
	store   *sqlite.Store
	control *runtimecontrol.Control
	manager *config.Manager
}

func newServiceAPI(t *testing.T) *serviceFixture {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "comporta.yaml")
	if err := os.WriteFile(path, []byte(serviceConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := config.NewManager(config.ManagerOptions{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	identity, err := useridentity.Open(context.Background(), useridentity.Options{Dir: manager.Capture().DataDir()})
	if err != nil {
		t.Fatal(err)
	}
	store, err := sqlite.Open(context.Background(), manager.Capture().DataDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	control := runtimecontrol.New(store)
	if err := control.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	clock := newFakeClock()
	service := NewService(ServiceOptions{Control: control, Source: manager, Now: clock.Now, Identity: identity, History: store, Usage: store, Limits: noLimits{}})
	api := newAPI(t, service)
	api.clock = clock
	return &serviceFixture{apiFixture: api, service: service, store: store, control: control, manager: manager}
}

func decode[T any](t *testing.T, body []byte) T {
	t.Helper()
	var value T
	if err := json.Unmarshal(body, &value); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	return value
}

func TestAdminPauseRoutes(t *testing.T) {
	f := newServiceAPI(t)
	post := func(path, body string) (int, map[string]any) {
		response := f.do(call{method: http.MethodPost, target: path, body: body, header: f.bearer()})
		return response.Code, decode[map[string]any](t, response.Body.Bytes())
	}
	status, body := post("/admin/v1/pause", `{"app":"bot-discord"}`)
	if status != 200 || body["app"] != "bot-discord" || body["app_paused"] != true || body["effective_paused"] != true || body["global_paused"] != false || body["changed"] != true || body["updated_at"] != "2026-11-08T03:12:44Z" {
		t.Fatalf("pause: %d %v", status, body)
	}
	if status, body = post("/admin/v1/pause", `{"app":"bot-discord"}`); status != 200 || body["changed"] != false || body["updated_at"] != "2026-11-08T03:12:44Z" {
		t.Fatalf("repeated pause: %d %v", status, body)
	}
	if status, body = post("/admin/v1/pause", `{"all":true}`); status != 200 || body["all"] != true || body["global_paused"] != true || body["app"] != nil {
		t.Fatalf("pause all: %d %v", status, body)
	}
	if status, body = post("/admin/v1/resume", `{"app":"bot-discord"}`); status != 200 || body["app_paused"] != false || body["effective_paused"] != true {
		t.Fatalf("resume under a global pause: %d %v", status, body)
	}
	response := f.do(call{method: http.MethodGet, target: "/admin/v1/pause", header: f.bearer()})
	state := decode[pauseStateResponse](t, response.Body.Bytes())
	if response.Code != 200 || !state.GlobalPaused || len(state.Apps) != 0 {
		t.Fatalf("state: %d %s", response.Code, response.Body)
	}
	if status, body = post("/admin/v1/resume", `{"all":true}`); status != 200 || body["global_paused"] != false || body["changed"] != true {
		t.Fatalf("resume all: %d %v", status, body)
	}
	for _, bad := range []string{`{}`, `{"all":false}`, `{"app":"bot-discord","all":true}`, `{"app":"Bot Discord"}`, `{"app":""}`, `{"app":7}`} {
		if status, _ := post("/admin/v1/pause", bad); status != 400 {
			t.Fatalf("%s: %d", bad, status)
		}
	}
	response = f.do(call{method: http.MethodPost, target: "/admin/v1/pause", body: `{"app":"unknown-app"}`, header: f.bearer()})
	if response.Code != 404 || errorCode(t, response) != "app_not_found" {
		t.Fatalf("unknown app: %d %s", response.Code, response.Body)
	}
	if response := f.do(call{method: http.MethodGet, target: "/admin/v1/resume", header: f.bearer()}); response.Code != 405 {
		t.Fatalf("an operation by GET: %d", response.Code)
	}
}

func TestResumeAcceptsOverrideOfRemovedApp(t *testing.T) {
	f := newServiceAPI(t)
	if _, err := f.control.Apply(context.Background(), pauseChange("removed-app")); err != nil {
		t.Fatal(err)
	}
	response := f.do(call{method: http.MethodPost, target: "/admin/v1/resume", body: `{"app":"removed-app"}`, header: f.bearer()})
	if response.Code != 200 {
		t.Fatalf("resume of a removed app with an override: %d %s", response.Code, response.Body)
	}
	response = f.do(call{method: http.MethodPost, target: "/admin/v1/pause", body: `{"app":"removed-app"}`, header: f.bearer()})
	if response.Code != 404 {
		t.Fatalf("pause of an app that is not configured: %d", response.Code)
	}
}

func TestPauseWithSessionNeedsOrigin(t *testing.T) {
	f := newServiceAPI(t)
	cookie := loginCookie(t, f.apiFixture)
	header := jsonHeader("Cookie", cookie.Name+"="+cookie.Value)
	if response := f.do(call{method: http.MethodPost, target: "/admin/v1/pause", body: `{"app":"other"}`, header: header}); response.Code != http.StatusForbidden {
		t.Fatalf("a cookie pause without Origin: %d", response.Code)
	}
	if response := f.do(call{method: http.MethodGet, target: "/admin/v1/pause", header: header}); response.Code != http.StatusOK {
		t.Fatalf("a cookie read: %d", response.Code)
	}
	header.Set("Origin", testOrigin)
	if response := f.do(call{method: http.MethodPost, target: "/admin/v1/pause", body: `{"app":"other"}`, header: header}); response.Code != http.StatusOK {
		t.Fatalf("a cookie pause with Origin: %d %s", response.Code, response.Body)
	}
	if _, err := f.control.Admit(runtimecontrol.Subject{App: "other"}); err == nil {
		t.Fatal("the pause was acknowledged without applying")
	}
}
