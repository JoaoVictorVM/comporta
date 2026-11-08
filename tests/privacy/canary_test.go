// Package privacy runs marked traffic through the gateway and examines everything it
// produces. A finding reports only the channel and the case, never the content.
package privacy

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/auth"
	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/keys"
	"github.com/JoaoVictorVM/comporta/internal/proxy"
	"github.com/JoaoVictorVM/comporta/internal/storage"
	"github.com/JoaoVictorVM/comporta/internal/storage/sqlite"
	"github.com/JoaoVictorVM/comporta/internal/testkit"
	"github.com/JoaoVictorVM/comporta/internal/useridentity"
)

const providerKeyEnv = "COMPORTA_CANARY_PROVIDER_KEY"

// suite records what actually ran, which the last test compares with the inventory: a
// scanner that read nothing, or a case that stopped running, must fail the build.
type suite struct {
	scanner      *testkit.Scanner
	mu           sync.Mutex
	cases        map[string]bool
	constructors map[string]int
}

var run = &suite{
	scanner:      testkit.NewScanner(testkit.Needles()...),
	cases:        map[string]bool{},
	constructors: map[string]int{},
}

func (s *suite) markCase(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cases[name] = true
}

func (s *suite) markConstructor(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.constructors[name]++
}

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	return listener.Addr().(*net.TCPAddr).Port
}

var (
	binaryOnce sync.Once
	binaryPath string
	binaryErr  error
)

func buildBinary(t *testing.T) string {
	t.Helper()
	binaryOnce.Do(func() {
		dir, err := os.MkdirTemp("", "comporta-canary-")
		if err != nil {
			binaryErr = err
			return
		}
		name := "comporta"
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		binaryPath = filepath.Join(dir, name)
		command := exec.Command("go", "build", "-o", binaryPath, "./cmd/comporta")
		command.Dir = repoRoot(t)
		if output, err := command.CombinedOutput(); err != nil {
			binaryErr = fmt.Errorf("%w: %s", err, output)
		}
	})
	if binaryErr != nil {
		t.Fatal(binaryErr)
	}
	return binaryPath
}

// liveGateway is the real binary serving against the real fake provider, with the
// output of both captured in files.
type liveGateway struct {
	stopOnce  sync.Once
	t         *testing.T
	dir       string
	proxyPort int
	key       string
	strictKey string
	processes []*exec.Cmd
	logs      []string
}

func (g *liveGateway) start(args ...string) *exec.Cmd {
	g.t.Helper()
	name := fmt.Sprintf("process-%d", len(g.processes))
	stdout, err := os.Create(filepath.Join(g.dir, name+".stdout"))
	if err != nil {
		g.t.Fatal(err)
	}
	stderr, err := os.Create(filepath.Join(g.dir, name+".stderr"))
	if err != nil {
		g.t.Fatal(err)
	}
	command := exec.Command(buildBinary(g.t), args...)
	command.Env = append(os.Environ(), providerKeyEnv+"=synthetic-provider-key")
	command.Stdout, command.Stderr = stdout, stderr
	if err := command.Start(); err != nil {
		g.t.Fatal(err)
	}
	g.processes = append(g.processes, command)
	g.logs = append(g.logs, stdout.Name(), stderr.Name())
	g.t.Cleanup(func() {
		_ = stdout.Close()
		_ = stderr.Close()
	})
	return command
}

func waitForPort(t *testing.T, port int) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		connection, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
		if err == nil {
			_ = connection.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("port %d did not open", port)
}

func startLive(t *testing.T, scenario, extraListeners string) *liveGateway {
	t.Helper()
	gateway := &liveGateway{t: t, dir: t.TempDir(), proxyPort: freePort(t)}
	fakePort, adminPort := freePort(t), freePort(t)
	config := filepath.Join(gateway.dir, "comporta.yaml")
	text := fmt.Sprintf(`timezone: UTC
providers:
  anthropic:
    api_format: anthropic
    base_url: http://127.0.0.1:%[1]d
    api_key: ${%[2]s}
    models: [fake-claude]
  openai:
    api_format: openai
    base_url: http://127.0.0.1:%[1]d/v1
    api_key: ${%[2]s}
    models: [fake-gpt]
pricing:
  fake-claude: {input: 2, output: 6, cache_write: 2.5, cache_read: 0.2}
apps:
  bot:
    allowed_models: [fake-claude, fake-gpt]
  strict:
    allowed_models: [fake-claude, fake-gpt]
    require_user: true
listeners:
  proxy: {port: %[3]d}
  admin: {port: %[4]d}
%[5]s`, fakePort, providerKeyEnv, gateway.proxyPort, adminPort, extraListeners)
	if err := os.WriteFile(config, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	gateway.key = gateway.createKey(config, "bot")
	gateway.strictKey = gateway.createKey(config, "strict")
	gateway.start("fake-provider", "--port", fmt.Sprint(fakePort), "--scenario", scenario)
	waitForPort(t, fakePort)
	gateway.start("serve", "--config", config)
	waitForPort(t, gateway.proxyPort)
	return gateway
}

func (g *liveGateway) createKey(config, app string) string {
	g.t.Helper()
	command := exec.Command(buildBinary(g.t), "keys", "create", app, "--config", config)
	command.Env = append(os.Environ(), providerKeyEnv+"=synthetic-provider-key")
	output, err := command.CombinedOutput()
	if err != nil {
		g.t.Fatalf("keys create: %v", err)
	}
	key := regexp.MustCompile(`cmp_[0-9A-Za-z]{43}`).FindString(string(output))
	if key == "" {
		g.t.Fatal("no key printed")
	}
	return key
}

func (g *liveGateway) url(path string) string {
	return fmt.Sprintf("http://127.0.0.1:%d%s", g.proxyPort, path)
}

func newCall(t *testing.T, ctx context.Context, url, key, user, body string) *http.Request {
	t.Helper()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	if strings.Contains(url, "/v1/messages") {
		request.Header.Set("X-Api-Key", key)
		request.Header.Set("Anthropic-Version", "2023-06-01")
	} else {
		request.Header.Set("Authorization", "Bearer "+key)
	}
	if user != "" {
		request.Header.Set("X-Comporta-User", user)
	}
	return request
}

// send makes a request and scans the answer. It returns the status, or zero when the
// connection broke, which some scenarios are about.
func (g *liveGateway) send(name, path, key, user, body string) int {
	g.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	response, err := http.DefaultClient.Do(newCall(g.t, ctx, g.url(path), key, user, body))
	if err != nil {
		return 0
	}
	defer func() { _ = response.Body.Close() }()
	data, _ := io.ReadAll(response.Body)
	run.scanner.Scan("errors", name, data)
	return response.StatusCode
}

// stop ends the processes and scans all they left: outputs and generated files.
func (g *liveGateway) stop() {
	g.t.Helper()
	g.stopOnce.Do(func() {
		for _, process := range g.processes {
			_ = process.Process.Kill()
		}
		for _, process := range g.processes {
			_ = process.Wait()
		}
		// Windows byte-range locks can prevent scanning a live SHM. Killing the
		// processes releases those locks without checkpointing away the WAL evidence.
		for _, name := range []string{"usage.sqlite", "usage.sqlite-wal", "usage.sqlite-shm"} {
			path := filepath.Join(g.dir, "data", name)
			if _, err := os.Stat(path); err == nil {
				run.scanner.ScanFile("database", path)
			} else {
				g.t.Error("required SQLite artifact missing")
			}
		}
		for _, path := range g.logs {
			run.scanner.ScanFile("logs", path)
		}
		run.scanner.ScanDir("files", filepath.Join(g.dir, "data"))
	})
}

func TestSQLiteJournalAndEventsContainNoContentOrRawID(t *testing.T) {
	run.markCase("process/database-recovery")
	g := startLive(t, "success", "")
	defer g.stop()
	run.markConstructor("AnthropicBody")
	if status := g.send("database-recovery", "/v1/messages", g.key, testkit.UserMarker(), testkit.AnthropicBody()); status != 200 {
		t.Fatalf("status %d", status)
	}
	reader, err := sqlite.OpenReader(context.Background(), filepath.Join(g.dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rows, err := reader.Daily(context.Background(), storage.Filter{AppName: "bot"})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	rows, err := reader.Daily(context.Background(), storage.Filter{AppName: "bot"})
	reader.Close()
	if err != nil || len(rows) != 1 || rows[0].Requests != 1 {
		t.Fatal("positive control: persisted usage missing")
	}
	g.stop()
	store, err := sqlite.Open(context.Background(), filepath.Join(g.dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Recover(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if err = store.Checkpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(g.dir, "data", "usage.sqlite")
	run.scanner.ScanFile("database", path)
	// SQL projections are checked in addition to bytes, including nonempty aggregates
	// and events, so a scan of an empty file cannot satisfy this channel.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, table := range []string{"requests", "usage_daily", "events"} {
		var n int
		if err = db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil || n == 0 {
			t.Fatal("positive control: required table empty")
		}
	}
}

type routeCall struct {
	constructor string
	path        string
}

var allRoutes = []routeCall{
	{"AnthropicBody", "/v1/messages"},
	{"AnthropicStreamBody", "/v1/messages"},
	{"CountBody", "/v1/messages/count_tokens"},
	{"OpenAIBody", "/v1/chat/completions"},
	{"OpenAIStreamBody", "/v1/chat/completions"},
}

func (g *liveGateway) sendAll(name string) {
	g.t.Helper()
	constructors := testkit.Constructors()
	for _, route := range allRoutes {
		g.send(name, route.path, g.key, testkit.UserMarker(), constructors[route.constructor]())
		run.markConstructor(route.constructor)
	}
}

func TestCanaryProcessScenarios(t *testing.T) {
	for _, scenario := range []string{"success", "rate-limit", "overload", "disconnect-midstream"} {
		t.Run(scenario, func(t *testing.T) {
			gateway := startLive(t, scenario, "")
			gateway.sendAll(scenario)
			gateway.stop()
			run.markCase("process/" + scenario)
		})
	}
	t.Run("header-timeout", func(t *testing.T) {
		gateway := startLive(t, "header-timeout", "  upstream_header_timeout: 200ms\n")
		if status := gateway.send("header-timeout", "/v1/chat/completions", gateway.key, testkit.UserMarker(), testkit.OpenAIBody()); status != http.StatusGatewayTimeout {
			t.Errorf("status %d", status)
		}
		gateway.stop()
		run.markCase("process/header-timeout")
	})
	t.Run("client-disconnect", func(t *testing.T) {
		gateway := startLive(t, "slow-stream", "")
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		response, err := http.DefaultClient.Do(newCall(t, ctx, gateway.url("/v1/chat/completions"), gateway.key, testkit.UserMarker(), testkit.OpenAIStreamBody()))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := bufio.NewReader(response.Body).ReadString('\n'); err != nil {
			t.Fatal(err)
		}
		cancel()
		_ = response.Body.Close()
		gateway.stop()
		run.markCase("process/client-disconnect")
	})
}

func TestCanaryRejectedRequests(t *testing.T) {
	gateway := startLive(t, "success", "")
	chat := "/v1/chat/completions"
	messages := "/v1/messages"
	t.Run("invalid identity", func(t *testing.T) {
		for name, user := range map[string]string{"email": testkit.EmailID(), "spaced": testkit.SpacedID(), "document": testkit.DocumentID()} {
			if status := gateway.send("identity "+name, chat, gateway.key, user, testkit.OpenAIBody()); status != http.StatusBadRequest {
				t.Errorf("%s: status %d", name, status)
			}
			if status := gateway.send("identity "+name, messages, gateway.key, user, testkit.AnthropicBody()); status != http.StatusBadRequest {
				t.Errorf("%s: status %d", name, status)
			}
		}
		body := testkit.WithField(testkit.OpenAIBody(), `"user":"`+testkit.EmailID()+`"`)
		if status := gateway.send("identity in body", chat, gateway.key, "", body); status != http.StatusBadRequest {
			t.Errorf("status %d", status)
		}
		run.markCase("process/invalid-identity")
	})
	t.Run("missing user", func(t *testing.T) {
		if status := gateway.send("missing user", chat, gateway.strictKey, "", testkit.OpenAIBody()); status != http.StatusBadRequest {
			t.Errorf("status %d", status)
		}
		run.markCase("process/missing-user")
	})
	t.Run("invalid request", func(t *testing.T) {
		broken := testkit.OpenAIBody()
		for name, body := range map[string]string{
			"truncated":      broken[:len(broken)-5],
			"unknown model":  strings.Replace(broken, "fake-gpt", "not-declared", 1),
			"duplicate key":  testkit.WithField(broken, `"user":"a","user":"`+testkit.UserMarker()+`"`),
			"wrong type":     testkit.WithField(broken, `"stream":"`+testkit.Marker()+`"`),
			"user wrong":     testkit.WithField(broken, `"user":{"id":"`+testkit.UserMarker()+`"}`),
			"metadata wrong": strings.Replace(testkit.AnthropicBody(), `"stream":false`, `"metadata":"`+testkit.Marker()+`"`, 1),
		} {
			path := chat
			if name == "metadata wrong" {
				path = messages
			}
			if status := gateway.send("invalid "+name, path, gateway.key, "", body); status != http.StatusBadRequest {
				t.Errorf("%s: status %d", name, status)
			}
		}
		run.markCase("process/invalid-request")
	})
	gateway.stop()
}

// pipeline is the handler in this process, for what a real process cannot show: a
// panic with content in it, a failing consumer, and what the provider actually got.
type pipeline struct {
	server   *httptest.Server
	key      string
	logs     *bytes.Buffer
	logMu    *sync.Mutex
	results  *collector
	identity *useridentity.Service
	dir      string
}

type collector struct {
	mu      sync.Mutex
	results []proxy.AttemptResult
	failure func() error
}

func (c *collector) Publish(_ context.Context, result proxy.AttemptResult) error {
	c.mu.Lock()
	c.results = append(c.results, result)
	failure := c.failure
	c.mu.Unlock()
	if failure != nil {
		return failure()
	}
	return nil
}

type lockedWriter struct {
	mu  *sync.Mutex
	out *bytes.Buffer
}

func (w lockedWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.out.Write(data)
}

func startPipeline(t *testing.T, upstreamURL, listeners string, hooks proxy.Hooks) *pipeline {
	t.Helper()
	t.Setenv(providerKeyEnv, "synthetic-provider-key")
	dir := t.TempDir()
	path := filepath.Join(dir, "comporta.yaml")
	text := fmt.Sprintf(`timezone: UTC
providers:
  openai:
    api_format: openai
    base_url: %s
    api_key: ${%s}
    models: [fake-gpt]
  anthropic:
    api_format: anthropic
    base_url: %s
    api_key: ${%s}
    models: [fake-claude]
pricing:
  fake-claude: {input: 2, output: 6, cache_write: 2.5, cache_read: 0.2}
apps:
  bot:
    allowed_models: [fake-gpt, fake-claude]
%s`, upstreamURL, providerKeyEnv, upstreamURL, providerKeyEnv, listeners)
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := config.NewManager(config.ManagerOptions{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	dataDir := manager.Capture().DataDir()
	created, err := keys.NewStore(dataDir).Create(context.Background(), "bot")
	if err != nil {
		t.Fatal(err)
	}
	keyManager, err := keys.NewManager(keys.ManagerOptions{Dir: dataDir})
	if err != nil {
		t.Fatal(err)
	}
	logs, logMu := &bytes.Buffer{}, &sync.Mutex{}
	logger := slog.New(slog.NewJSONHandler(lockedWriter{logMu, logs}, nil))
	identity, err := useridentity.Open(context.Background(), useridentity.Options{Dir: dataDir, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	results := &collector{}
	handler := proxy.NewHandler(proxy.Options{
		Auth:  auth.NewHTTP(keyManager, auth.NewAuthenticator(keyManager, nil), auth.NewGuard(nil, logger)),
		Sink:  results,
		Hooks: hooks, Identity: identity, Logger: logger,
	})
	t.Cleanup(handler.Close)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target, _, _ := strings.Cut(r.RequestURI, "?")
		route, ok := proxy.MatchRoute(r.Method, target)
		if !ok {
			proxy.WriteFailure(w, auth.ModelsFormat(r), proxy.EndpointNotAllowed())
			return
		}
		handler.ServeRoute(w, r, route, manager.Capture())
	}))
	t.Cleanup(server.Close)
	return &pipeline{
		server: server, key: created.Key.Reveal(), logs: logs, logMu: logMu,
		results: results, identity: identity, dir: dataDir,
	}
}

func (p *pipeline) post(t *testing.T, path, body string) (int, []byte) {
	t.Helper()
	response, err := http.DefaultClient.Do(newCall(t, context.Background(), p.server.URL+path, p.key, testkit.UserMarker(), body))
	if err != nil {
		return 0, nil
	}
	defer func() { _ = response.Body.Close() }()
	data, _ := io.ReadAll(response.Body)
	return response.StatusCode, data
}

// scanAll examines each destination of this pipeline.
func (p *pipeline) scanAll(t *testing.T, name string, answer []byte) {
	t.Helper()
	run.scanner.Scan("errors", name, answer)
	p.logMu.Lock()
	run.scanner.Scan("logs", name, slices.Clone(p.logs.Bytes()))
	p.logMu.Unlock()
	p.results.mu.Lock()
	defer p.results.mu.Unlock()
	if len(p.results.results) == 0 {
		t.Errorf("%s: no attempt result to inspect", name)
	}
	for _, result := range p.results.results {
		serialized, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		run.scanner.Scan("results", name, serialized)
		run.scanner.Scan("results", name, fmt.Appendf(nil, "%v %+v %#v", result, result, result))
		if result.UserHash.IsZero() {
			t.Errorf("%s: a result without the identity", name)
		}
	}
	run.scanner.ScanDir("files", p.dir)
	run.markCase("pipeline/" + name)
}

// echoUpstream answers with whatever the provider read, as a provider echoing a prompt
// in an error would.
func echoUpstream(t *testing.T, write func(w http.ResponseWriter, body []byte)) (*httptest.Server, *lastBody) {
	t.Helper()
	last := &lastBody{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		last.set(body, r.Header)
		write(w, body)
	}))
	t.Cleanup(server.Close)
	return server, last
}

type lastBody struct {
	mu     sync.Mutex
	body   []byte
	header http.Header
}

func (l *lastBody) set(body []byte, header http.Header) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.body, l.header = bytes.Clone(body), header.Clone()
}

func (l *lastBody) get() ([]byte, http.Header) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.body, l.header
}

func quoteJSON(data []byte) string {
	encoded, _ := json.Marshal(string(data))
	return string(encoded)
}

func TestCanaryProviderReceivesPromptNotUser(t *testing.T) {
	upstream, last := echoUpstream(t, func(w http.ResponseWriter, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	})
	p := startPipeline(t, upstream.URL, "", proxy.Hooks{})
	status, answer := p.post(t, "/v1/chat/completions", testkit.WithField(testkit.OpenAIBody(), `"metadata":{"user_id":"`+testkit.UserMarker()+`"}`))
	if status != http.StatusOK {
		t.Fatalf("status %d", status)
	}
	// Payloads to the provider are an intended channel for content, so this inspects
	// them in memory and asserts the transport instead of adding them to the scanner.
	sent, header := last.get()
	if !bytes.Contains(sent, []byte(testkit.Marker())) {
		t.Error("the control failed: the prompt must reach the provider")
	}
	if bytes.Contains(sent, []byte(testkit.UserMarker())) {
		t.Error("the raw identifier reached the provider")
	}
	if strings.Contains(fmt.Sprint(header), testkit.UserMarker()) {
		t.Error("the raw identifier reached the provider headers")
	}
	hash, err := p.identity.LookupHash("bot", testkit.UserMarker())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(sent, []byte(hash.Hex())) {
		t.Error("the provider did not receive the pseudonym")
	}
	p.scanAll(t, "provider-receives-prompt", answer)
}

func TestCanaryUpstreamEchoes(t *testing.T) {
	t.Run("error body", func(t *testing.T) {
		upstream, _ := echoUpstream(t, func(w http.ResponseWriter, body []byte) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprintf(w, `{"error":{"message":%s,"type":"invalid_request_error"}}`, quoteJSON(body))
		})
		p := startPipeline(t, upstream.URL, "", proxy.Hooks{})
		status, answer := p.post(t, "/v1/chat/completions", testkit.OpenAIBody())
		if status != http.StatusBadRequest {
			t.Fatalf("status %d", status)
		}
		p.scanAll(t, "upstream-error-echo", answer)
	})
	t.Run("stream error event", func(t *testing.T) {
		upstream, _ := echoUpstream(t, func(w http.ResponseWriter, body []byte) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprintf(w, "data: {\"id\":\"c\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}],\"usage\":null}\n\ndata: {\"error\":{\"message\":%s,\"type\":\"server_error\"}}\n\n", quoteJSON(body))
		})
		p := startPipeline(t, upstream.URL, "", proxy.Hooks{})
		_, answer := p.post(t, "/v1/chat/completions", testkit.OpenAIStreamBody())
		p.scanAll(t, "stream-error-echo", answer)
	})
}

func TestCanaryPanicAndConsumerFailure(t *testing.T) {
	t.Run("panic carrying content", func(t *testing.T) {
		upstream, _ := echoUpstream(t, func(w http.ResponseWriter, _ []byte) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))
		})
		p := startPipeline(t, upstream.URL, "", proxy.Hooks{
			BeforeSend: func(_ context.Context, _ *proxy.Envelope, content proxy.RequestContent) *proxy.Rejection {
				panic(string(content.Body))
			},
		})
		status, answer := p.post(t, "/v1/chat/completions", testkit.OpenAIBody())
		if status != http.StatusInternalServerError {
			t.Fatalf("status %d", status)
		}
		run.scanner.Scan("errors", "hook-panic", answer)
		p.logMu.Lock()
		logged := slices.Clone(p.logs.Bytes())
		p.logMu.Unlock()
		if !bytes.Contains(logged, []byte("panicked")) {
			t.Error("the control failed: the panic must be logged")
		}
		run.scanner.Scan("logs", "hook-panic", logged)
		run.scanner.ScanDir("files", p.dir)
		run.markCase("pipeline/hook-panic")
	})
	t.Run("failing consumer", func(t *testing.T) {
		upstream, _ := echoUpstream(t, func(w http.ResponseWriter, _ []byte) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))
		})
		p := startPipeline(t, upstream.URL, "", proxy.Hooks{})
		p.results.failure = func() error { return errors.New(testkit.Marker()) }
		status, answer := p.post(t, "/v1/chat/completions", testkit.OpenAIBody())
		if status != http.StatusOK {
			t.Fatalf("status %d", status)
		}
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			p.logMu.Lock()
			logged := bytes.Contains(p.logs.Bytes(), []byte("consumer failed"))
			p.logMu.Unlock()
			if logged {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		p.scanAll(t, "sink-failure", answer)
	})
}

func TestCanaryTimeoutAndDisconnect(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		release := make(chan struct{})
		upstream, _ := echoUpstream(t, func(http.ResponseWriter, []byte) { <-release })
		defer close(release)
		p := startPipeline(t, upstream.URL, "listeners:\n  upstream_header_timeout: 100ms\n", proxy.Hooks{})
		status, answer := p.post(t, "/v1/chat/completions", testkit.OpenAIBody())
		if status != http.StatusGatewayTimeout {
			t.Fatalf("status %d", status)
		}
		p.scanAll(t, "timeout", answer)
	})
	t.Run("client disconnect", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"id\":\"c\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}],\"usage\":null}\n\n"))
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		}))
		defer upstream.Close()
		p := startPipeline(t, upstream.URL, "", proxy.Hooks{})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		response, err := http.DefaultClient.Do(newCall(t, ctx, p.server.URL+"/v1/chat/completions", p.key, testkit.UserMarker(), testkit.OpenAIStreamBody()))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := bufio.NewReader(response.Body).ReadString('\n'); err != nil {
			t.Fatal(err)
		}
		cancel()
		_ = response.Body.Close()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			p.results.mu.Lock()
			published := len(p.results.results)
			p.results.mu.Unlock()
			if published > 0 {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		p.scanAll(t, "client-disconnect", nil)
	})
}

type failingEntropy struct{}

func (failingEntropy) Read([]byte) (int, error) { return 0, errors.New(testkit.Marker()) }

func TestCanaryKeyFileFailure(t *testing.T) {
	var logs bytes.Buffer
	dir := filepath.Join(t.TempDir(), testkit.Marker())
	_, err := useridentity.Open(context.Background(), useridentity.Options{
		Dir: dir, Entropy: failingEntropy{}, Logger: slog.New(slog.NewJSONHandler(&logs, nil)),
	})
	if err == nil {
		t.Fatal("expected a failure")
	}
	run.scanner.Scan("logs", "keyfile-failure", logs.Bytes())
	run.scanner.Scan("errors", "keyfile-failure", []byte(err.Error()))
	run.markCase("startup/keyfile-failure")
}

// promptLiteral finds literal prompts, in Go or Python spelling.
var promptLiteral = regexp.MustCompile(`"role"\s*:`)

func TestCanaryInventoryMatchesTheRepository(t *testing.T) {
	root := repoRoot(t)
	inventory, err := testkit.ReadInventory(filepath.Join(root, "tests", "privacy", "coverage.json"))
	if err != nil {
		t.Fatal(err)
	}

	var names []string
	for name, build := range testkit.Constructors() {
		names = append(names, name)
		if !strings.Contains(build(), testkit.Marker()) {
			t.Errorf("constructor %s has no marker", name)
		}
	}
	slices.Sort(names)
	listed := slices.Sorted(slices.Values(inventory.Constructors))
	if !slices.Equal(names, listed) {
		t.Errorf("constructors in the code %v, in the inventory %v", names, listed)
	}

	parserOnly := map[string]bool{}
	for _, entry := range inventory.ParserOnlyGo {
		if strings.TrimSpace(entry.Reason) == "" {
			t.Errorf("%s has no reason to stay unmarked", entry.File)
		}
		parserOnly[entry.File] = true
	}
	instrumented := map[string]bool{}
	for _, file := range inventory.InstrumentedGo {
		instrumented[file] = true
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(file)))
		if err != nil {
			t.Errorf("listed file %s: %v", file, err)
			continue
		}
		if !bytes.Contains(data, []byte("testkit.")) {
			t.Errorf("%s is listed as instrumented and does not use the testkit", file)
		}
	}
	for _, base := range []string{"internal", "tests", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, base), func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil || entry.IsDir() || !strings.HasSuffix(path, "_test.go") {
				return walkErr
			}
			relative, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			relative = filepath.ToSlash(relative)
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if promptLiteral.Match(data) && !instrumented[relative] && !parserOnly[relative] {
				t.Errorf("%s builds prompts and is not in the inventory: use the testkit or list it", relative)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	sdk, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(inventory.InstrumentedSDK)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(sdk, []byte("def prompt(")) {
		t.Error("the SDK suite has no marked prompt helper")
	}
	if regexp.MustCompile(`"(content|text)":\s*"[^"]`).Match(sdk) {
		t.Error("the SDK suite builds a prompt without the helper")
	}
}

func TestCanaryTestkitStaysOutOfTheBinary(t *testing.T) {
	command := exec.Command("go", "list", "-deps", "./cmd/comporta")
	command.Dir = repoRoot(t)
	output, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(output), "internal/testkit") {
		t.Fatal("the binary depends on test infrastructure")
	}
	if !strings.Contains(string(output), "internal/useridentity") {
		t.Fatal("the control failed: the listing does not look like the binary")
	}
}

// TestCanaryZZZResult must run last: Go runs tests in source order, and it judges what
// the others did. The failure names channels and cases only.
func TestCanaryZZZResult(t *testing.T) {
	inventory, err := testkit.ReadInventory(filepath.Join(repoRoot(t), "tests", "privacy", "coverage.json"))
	if err != nil {
		t.Fatal(err)
	}
	var required []string
	for _, channel := range inventory.Channels {
		switch channel.Status {
		case testkit.ChannelCovered:
			required = append(required, channel.Name)
		case testkit.ChannelUnavailable:
			if channel.RequiredBy == "" {
				t.Errorf("channel %s is unavailable and names no owner", channel.Name)
			}
			if run.scanner.Inspected(channel.Name) != 0 {
				t.Errorf("channel %s has outputs now: mark it as covered", channel.Name)
			}
		default:
			t.Errorf("channel %s has status %q", channel.Name, channel.Status)
		}
	}
	if err := run.scanner.Err(required...); err != nil {
		t.Error(err)
	}
	if t.Failed() {
		return
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	for _, name := range inventory.Cases {
		if !run.cases[name] {
			t.Errorf("case %s did not run", name)
		}
	}
	for name := range run.cases {
		if !slices.Contains(inventory.Cases, name) {
			t.Errorf("case %s ran and is not in the inventory", name)
		}
	}
	for _, name := range inventory.Constructors {
		if run.constructors[name] == 0 {
			t.Errorf("constructor %s was never sent", name)
		}
	}
}
