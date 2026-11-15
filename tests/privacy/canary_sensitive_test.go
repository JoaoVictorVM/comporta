package privacy

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
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/JoaoVictorVM/comporta/internal/auth"
	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/events"
	"github.com/JoaoVictorVM/comporta/internal/keys"
	"github.com/JoaoVictorVM/comporta/internal/metering"
	"github.com/JoaoVictorVM/comporta/internal/pricing"
	"github.com/JoaoVictorVM/comporta/internal/proxy"
	"github.com/JoaoVictorVM/comporta/internal/sensitivedata"
	"github.com/JoaoVictorVM/comporta/internal/storage"
	"github.com/JoaoVictorVM/comporta/internal/storage/sqlite"
	"github.com/JoaoVictorVM/comporta/internal/testkit"
	"github.com/JoaoVictorVM/comporta/internal/useridentity"
)

// Sensitive values of the canary. Each carries a marker, so the shared scanner
// finds any copy of them; the local scanner also looks for the CPF, which cannot
// carry one, and for the start of every placeholder.
func canaryKey() string   { return "sk-" + "ant-" + "api03-" + testkit.Marker() }
func canaryEmail() string { return testkit.EmailID() }
func canaryCPF() string   { return testkit.DocumentID() }

var sensitiveNeedles = []string{"[EMAIL_1_", "[CPF_1_", "[SECRET_1_"}

func sensitiveScanner() *testkit.Scanner {
	return testkit.NewScanner(append([]string{canaryCPF()}, sensitiveNeedles...)...)
}

func sensitiveContent() string {
	return testkit.Prompt("cliente " + canaryEmail() + " cpf " + canaryCPF())
}

func sensitiveAnthropic(text string) string {
	encoded, _ := json.Marshal(text)
	return `{"model":"fake-claude","max_tokens":16,"messages":[{"role":"user","content":` + string(encoded) + `}]}`
}

func detectionBlock(text string) string {
	anchor := "  bot:\n    allowed_models: [fake-claude, fake-gpt]\n"
	block := "    detection:\n      email: redact\n      cpf: redact\n      phone: detect\n      secret: block\n      rehydrate: true\n"
	return strings.Replace(text, anchor, anchor+block, 1)
}

func requireClean(t *testing.T, scanner *testkit.Scanner, channels ...string) {
	t.Helper()
	if findings := scanner.Findings(); len(findings) > 0 {
		t.Fatalf("sensitive values or placeholders found: %+v", findings)
	}
	for _, channel := range channels {
		if scanner.Inspected(channel) == 0 {
			t.Fatalf("nothing was inspected on %s", channel)
		}
	}
}

// TestSensitiveCanaryChannels runs the real processes with detection on: a redacted
// request, a refused secret and a failed upstream. Nothing they leave behind may
// hold a detected value or a placeholder.
func TestSensitiveCanaryChannels(t *testing.T) {
	run.markCase("process/sensitive-data")
	g := startLiveWith(t, "success", "", detectionBlock)
	local := sensitiveScanner()
	user := testkit.UserMarker()
	if status := g.send("sensitive-redact", "/v1/messages", g.key, user, sensitiveAnthropic(sensitiveContent())); status != http.StatusOK {
		t.Fatalf("redacted request: status %d", status)
	}
	if status := g.send("sensitive-block", "/v1/messages", g.key, user, sensitiveAnthropic(testkit.Prompt("chave "+canaryKey()))); status != http.StatusBadRequest {
		t.Fatalf("positive control: the secret was not refused, status %d", status)
	}
	reader, err := sqlite.OpenReader(context.Background(), filepath.Join(g.dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	page, err := reader.ReadEvents(context.Background(), events.Filter{Types: []events.Type{events.SensitiveDataDetected}})
	reader.Close()
	if err != nil || len(page.Events) != 2 {
		t.Fatalf("positive control: sensitive events %d %v", len(page.Events), err)
	}
	g.stop()
	for _, name := range []string{"usage.sqlite", "usage.sqlite-wal"} {
		if path := filepath.Join(g.dir, "data", name); fileExists(path) {
			local.ScanFile("database", path)
		}
	}
	for _, path := range g.logs {
		local.ScanFile("logs", path)
	}
	local.ScanDir("files", filepath.Join(g.dir, "data"))
	requireClean(t, local, "database", "logs", "files")
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// sensitivePipeline is the handler in this process with real metering and SQLite,
// for the paths a real process cannot show: a panic carrying content and the
// restored answer.
type sensitivePipeline struct {
	server  *httptest.Server
	key     string
	logs    *bytes.Buffer
	logMu   *sync.Mutex
	results *collector
	store   *sqlite.Store
	service *metering.Service
	dir     string
	bus     []events.Event
	busMu   sync.Mutex
}

func startSensitivePipeline(t *testing.T, upstreamURL string, scanner *sensitivedata.Scanner) *sensitivePipeline {
	t.Helper()
	t.Setenv(providerKeyEnv, "synthetic-provider-key")
	dir := t.TempDir()
	path := filepath.Join(dir, "comporta.yaml")
	text := detectionBlock(fmt.Sprintf(`timezone: UTC
providers:
  anthropic:
    api_format: anthropic
    base_url: %s
    api_key: ${%s}
    models: [fake-claude, fake-gpt]
pricing:
  fake-claude: {input: 2, output: 6, cache_write: 2.5, cache_read: 0.2}
apps:
  bot:
    allowed_models: [fake-claude, fake-gpt]
`, upstreamURL, providerKeyEnv))
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
	store, err := sqlite.Open(context.Background(), dataDir)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := pricing.Embedded()
	if err != nil {
		t.Fatal(err)
	}
	service := metering.New(store, catalog, logger, func() int64 { return 30 })
	p := &sensitivePipeline{key: created.Key.Reveal(), logs: logs, logMu: logMu, results: &collector{}, store: store, service: service, dir: dataDir}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sub := store.Bus.Subscribe(ctx)
	go func() {
		for e := range sub.Events {
			p.busMu.Lock()
			p.bus = append(p.bus, e.Clone())
			p.busMu.Unlock()
		}
	}()
	handler := proxy.NewHandler(proxy.Options{
		Auth:     auth.NewHTTP(keyManager, auth.NewAuthenticator(keyManager, nil), auth.NewGuard(nil, logger)),
		Sink:     p.results,
		Identity: identity, Logger: logger, Accounting: service,
		Sensitive: &proxy.SensitiveStage{Scanner: scanner, Recorder: service},
	})
	t.Cleanup(handler.Close)
	p.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target, _, _ := strings.Cut(r.RequestURI, "?")
		route, ok := proxy.MatchRoute(r.Method, target)
		if !ok {
			proxy.WriteFailure(w, auth.ModelsFormat(r), proxy.EndpointNotAllowed())
			return
		}
		handler.ServeRoute(w, r, route, manager.Capture())
	}))
	t.Cleanup(p.server.Close)
	return p
}

func (p *sensitivePipeline) post(t *testing.T, body string) (int, []byte) {
	t.Helper()
	response, err := http.DefaultClient.Do(newCall(t, context.Background(), p.server.URL+"/v1/messages", p.key, testkit.UserMarker(), body))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	data, _ := io.ReadAll(response.Body)
	return response.StatusCode, data
}

// scanStored examines logs, results, records, events, the bus and the database
// files, with both scanners, after the store has been checkpointed and closed.
func (p *sensitivePipeline) scanStored(t *testing.T, name string, local *testkit.Scanner) {
	t.Helper()
	if err := p.service.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	page, err := p.store.Records(context.Background(), storage.RecordFilter{AppName: "bot", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	stored, err := p.store.ReadEvents(context.Background(), events.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	p.busMu.Lock()
	bus := append([]events.Event(nil), p.bus...)
	p.busMu.Unlock()
	p.results.mu.Lock()
	results := append([]proxy.AttemptResult(nil), p.results.results...)
	p.results.mu.Unlock()
	dtos, _ := json.Marshal(struct {
		Records []storage.Record
		Events  []events.Event
		Bus     []events.Event
		Results []proxy.AttemptResult
	}{page.Records, stored.Events, bus, results})
	p.logMu.Lock()
	logs := bytes.Clone(p.logs.Bytes())
	p.logMu.Unlock()
	for _, scanner := range []*testkit.Scanner{run.scanner, local} {
		scanner.Scan("logs", name, logs)
		scanner.Scan("results", name, dtos)
		scanner.Scan("results", name, fmt.Appendf(nil, "%v %+v", results, results))
	}
	if err := p.store.Checkpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := p.store.Close(); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{"usage.sqlite", "usage.sqlite-wal"} {
		if path := filepath.Join(p.dir, file); fileExists(path) {
			run.scanner.ScanFile("database", path)
			local.ScanFile("database", path)
		}
	}
	run.scanner.ScanDir("files", p.dir)
	local.ScanDir("files", p.dir)
}

// panickingEntropy panics with request content while the placeholders are built,
// inside the scan.
type panickingEntropy struct{}

func (panickingEntropy) Read([]byte) (int, error) {
	panic("leak " + canaryEmail() + " " + canaryCPF())
}

// TestSensitivePanicCanary makes the inspection panic with content in the panic
// value: the answer is the fixed 503, and nothing of the value goes anywhere.
func TestSensitivePanicCanary(t *testing.T) {
	run.markCase("pipeline/sensitive-panic")
	upstream, last := echoUpstream(t, func(w http.ResponseWriter, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	})
	p := startSensitivePipeline(t, upstream.URL, sensitivedata.NewScanner(sensitivedata.WithEntropy(panickingEntropy{})))
	local := sensitiveScanner()
	status, answer := p.post(t, sensitiveAnthropic(sensitiveContent()))
	if status != http.StatusServiceUnavailable || !bytes.Contains(answer, []byte("sensitive_data_scan_failed")) {
		t.Fatalf("positive control: status %d %s", status, answer)
	}
	if body, _ := last.get(); body != nil {
		t.Fatal("the provider was called after a failed scan")
	}
	run.scanner.Scan("errors", "sensitive-panic", answer)
	local.Scan("errors", "sensitive-panic", answer)
	p.scanStored(t, "sensitive-panic", local)
	requireClean(t, local, "errors", "logs", "results", "database", "files")
}

// TestRehydratedValueNeverPersisted restores the values in the answer, which the
// client may see, and checks that no other destination ever received them.
func TestRehydratedValueNeverPersisted(t *testing.T) {
	run.markCase("pipeline/sensitive-rehydration")
	tokens := regexp.MustCompile(`\[[A-Z]+_\d+_[0-9a-f]{24}\]`)
	upstream, last := echoUpstream(t, func(w http.ResponseWriter, body []byte) {
		echoed := strings.Join(tokens.FindAllString(string(body), -1), " ")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","content":[{"type":"text","text":"de volta ` + echoed + `"}],"usage":{"input_tokens":5,"output_tokens":3}}`))
	})
	p := startSensitivePipeline(t, upstream.URL, sensitivedata.NewScanner())
	local := sensitiveScanner()
	status, answer := p.post(t, sensitiveAnthropic(sensitiveContent()))
	if status != http.StatusOK || !bytes.Contains(answer, []byte(canaryEmail())) || !bytes.Contains(answer, []byte(canaryCPF())) {
		t.Fatalf("positive control: the answer was not restored: %d %s", status, answer)
	}
	sent, _ := last.get()
	if bytes.Contains(sent, []byte(canaryEmail())) || bytes.Contains(sent, []byte(canaryCPF())) || !tokens.Match(sent) {
		t.Fatalf("the provider received a value: %s", sent)
	}
	p.scanStored(t, "sensitive-rehydration", local)
	requireClean(t, local, "logs", "results", "database", "files")
}
