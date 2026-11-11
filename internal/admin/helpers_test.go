package admin

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/storage"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 11, 8, 3, 12, 44, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type memoryMarker struct {
	mu          sync.Mutex
	initialized bool
	failMark    error
	marks       int
}

func (m *memoryMarker) AdminInitialized(context.Context) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.initialized, nil
}

func (m *memoryMarker) MarkAdminInitialized(context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failMark != nil {
		return m.failMark
	}
	m.initialized = true
	m.marks++
	return nil
}

var errMarkerDown = errors.New("marker storage down")

// newCredential creates a fresh credential and returns it with its token.
func newCredential(t *testing.T) (*Credential, string) {
	t.Helper()
	var token string
	credential, err := OpenCredential(context.Background(), t.TempDir(), &memoryMarker{}, func(value string) { token = value })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = credential.Close() })
	if token == "" {
		t.Fatal("no token shown on bootstrap")
	}
	return credential, token
}

type apiFixture struct {
	handler *Handler
	token   string
	clock   *fakeClock
}

func newAPI(t *testing.T, service *Service) *apiFixture {
	t.Helper()
	credential, token := newCredential(t)
	clock := newFakeClock()
	handler := NewHandler(HandlerOptions{
		Credential: credential, Sessions: NewSessions(clock.Now), Guard: NewGuard(clock, nil),
		Service: service, Now: clock.Now,
	})
	return &apiFixture{handler: handler, token: token, clock: clock}
}

const testOrigin = "http://localhost:8081"

type call struct {
	method, target, body string
	header               http.Header
	remote               string
}

func (f *apiFixture) do(c call) *httptest.ResponseRecorder {
	request := httptest.NewRequest(c.method, "http://localhost:8081"+c.target, strings.NewReader(c.body))
	request.RequestURI = c.target
	request.Host = "localhost:8081"
	request.RemoteAddr = "192.0.2.10:40000"
	if c.remote != "" {
		request.RemoteAddr = c.remote
	}
	if c.body == "" {
		request.ContentLength = 0
	}
	for name, values := range c.header {
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
	recorder := httptest.NewRecorder()
	f.handler.ServeHTTP(recorder, request)
	return recorder
}

func jsonHeader(extra ...string) http.Header {
	header := http.Header{"Content-Type": {"application/json"}}
	for i := 0; i+1 < len(extra); i += 2 {
		header.Add(extra[i], extra[i+1])
	}
	return header
}

func (f *apiFixture) bearer(extra ...string) http.Header {
	return jsonHeader(append([]string{"Authorization", "Bearer " + f.token}, extra...)...)
}

func errorCode(t *testing.T, recorder *httptest.ResponseRecorder) string {
	t.Helper()
	body := recorder.Body.String()
	start := strings.Index(body, `"code":"`)
	if start < 0 {
		t.Fatalf("no error code in %s", body)
	}
	rest := body[start+len(`"code":"`):]
	return rest[:strings.IndexByte(rest, '"')]
}

func pauseChange(app string) storage.PauseChange {
	return storage.PauseChange{App: app, Paused: true, At: time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)}
}

func configMicrousd(n int64) config.Microusd { return config.Microusd(n) }
