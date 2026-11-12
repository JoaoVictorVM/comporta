package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/auth"
	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/keys"
	"github.com/JoaoVictorVM/comporta/internal/useridentity"
)

const (
	providerCanary = "provider-secret-canary"
	configTemplate = `timezone: UTC
providers:
  anthropic:
    api_format: anthropic
    base_url: %[1]s
    api_key: ${COMPORTA_PROXY_TEST_KEY}
    models: [fake-claude]
  openai:
    api_format: openai
    base_url: %[1]s
    api_key: ${COMPORTA_PROXY_TEST_KEY}
    models: [fake-gpt]
apps:
  bot:
    allowed_models: [fake-claude, fake-gpt]
%[2]s`
)

// fixture is a handler on a real socket, with real key and configuration managers.
type fixture struct {
	t        testing.TB
	config   *config.Manager
	path     string
	key      string
	sink     *collector
	handler  *Handler
	identity *useridentity.Service
	server   *httptest.Server
}

type fixtureOptions struct {
	listeners   string
	hooks       Hooks
	logs        io.Writer
	requireUser bool
	gate        Gate
	loop        *LoopStage
	accounting  Accounting
}

func newFixture(t testing.TB, upstream string, options fixtureOptions) *fixture {
	t.Helper()
	t.Setenv("COMPORTA_PROXY_TEST_KEY", providerCanary)
	path := filepath.Join(t.TempDir(), "comporta.yaml")
	extra := options.listeners
	if options.requireUser {
		extra = "    require_user: true\n" + extra
	}
	content := fmt.Sprintf(configTemplate, upstream, extra)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := config.NewManager(config.ManagerOptions{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	created, err := keys.NewStore(manager.Capture().DataDir()).Create(context.Background(), "bot")
	if err != nil {
		t.Fatal(err)
	}
	keyManager, err := keys.NewManager(keys.ManagerOptions{Dir: manager.Capture().DataDir()})
	if err != nil {
		t.Fatal(err)
	}
	authHTTP := auth.NewHTTP(keyManager, auth.NewAuthenticator(keyManager, nil), auth.NewGuard(nil, nil))
	identity, err := useridentity.Open(context.Background(), useridentity.Options{Dir: manager.Capture().DataDir()})
	if err != nil {
		t.Fatal(err)
	}
	sink := &collector{}
	handler := newTestHandler(authHTTP, sink, identity, options)
	fx := &fixture{t: t, config: manager, path: path, key: created.Key.Reveal(), sink: sink, handler: handler, identity: identity}
	fx.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target, _, _ := strings.Cut(r.RequestURI, "?")
		route, ok := MatchRoute(r.Method, target)
		if !ok {
			WriteFailure(w, auth.ModelsFormat(r), EndpointNotAllowed())
			return
		}
		handler.ServeRoute(w, r, route, manager.Capture())
	}))
	t.Cleanup(fx.server.Close)
	return fx
}

// collector is a sink that keeps every result.
type collector struct {
	mu      sync.Mutex
	results []AttemptResult
	notify  chan struct{}
}

func (c *collector) Publish(_ context.Context, result AttemptResult) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.results = append(c.results, result)
	if c.notify != nil {
		close(c.notify)
		c.notify = nil
	}
	return nil
}

func (c *collector) all() []AttemptResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]AttemptResult(nil), c.results...)
}

// wait returns the only result, failing when none arrives or more than one does.
func (c *collector) wait(t *testing.T) AttemptResult {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if results := c.all(); len(results) > 0 {
			time.Sleep(20 * time.Millisecond)
			results = c.all()
			if len(results) != 1 {
				t.Fatalf("expected one result, got %d", len(results))
			}
			return results[0]
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no result was published")
	return AttemptResult{}
}

func (f *fixture) request(method, path, format, body string) *http.Request {
	f.t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request, err := http.NewRequest(method, f.server.URL+path, reader)
	if err != nil {
		f.t.Fatal(err)
	}
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	if format == "anthropic" {
		request.Header.Set("X-Api-Key", f.key)
	} else {
		request.Header.Set("Authorization", "Bearer "+f.key)
	}
	return request
}

func (f *fixture) do(request *http.Request) (*http.Response, string) {
	f.t.Helper()
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		f.t.Fatalf("reading the response: %v", err)
	}
	return response, string(data)
}

func (f *fixture) post(path, format, body string) (*http.Response, string) {
	f.t.Helper()
	return f.do(f.request(http.MethodPost, path, format, body))
}

func comportaCode(t *testing.T, body string) string {
	t.Helper()
	var parsed struct {
		Error struct {
			Code string `json:"comporta_code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("not a JSON error %q: %v", body, err)
	}
	return parsed.Error.Code
}

// recordingUpstream captures the last request and answers with a fixed response.
type recordingUpstream struct {
	mu       sync.Mutex
	requests int
	body     []byte
	header   http.Header
	path     string
	reply    http.HandlerFunc
}

func (u *recordingUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	u.mu.Lock()
	u.requests++
	u.body = bytes.Clone(body)
	u.header = r.Header.Clone()
	u.path = r.RequestURI
	reply := u.reply
	u.mu.Unlock()
	if reply != nil {
		reply(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"usage":{"input_tokens":1,"output_tokens":1}}`))
}

func (u *recordingUpstream) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.requests
}

func startUpstream(t *testing.T, reply http.HandlerFunc) (*recordingUpstream, *httptest.Server) {
	t.Helper()
	upstream := &recordingUpstream{reply: reply}
	server := httptest.NewServer(upstream)
	t.Cleanup(server.Close)
	return upstream, server
}

func newTestHandler(authHTTP *auth.HTTP, sink ResultSink, identity *useridentity.Service, options fixtureOptions) *Handler {
	logger := slog.New(slog.DiscardHandler)
	if options.logs != nil {
		logger = slog.New(slog.NewJSONHandler(options.logs, nil))
	}
	return NewHandler(Options{Auth: authHTTP, Sink: sink, Hooks: options.hooks, Identity: identity, Logger: logger, Gate: options.gate, Loop: options.loop, Accounting: options.accounting})
}

func startBenchUpstream(b *testing.B) string {
	b.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	b.Cleanup(server.Close)
	return server.URL
}

func newBenchFixture(b *testing.B, upstream string) *fixture {
	b.Helper()
	return newFixture(b, upstream, fixtureOptions{})
}
