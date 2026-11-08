// Package integration exercises the gateway through real sockets, with the real
// configuration and key managers and the built-in fake provider.
package integration

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
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
	"github.com/JoaoVictorVM/comporta/internal/fakeprovider"
	"github.com/JoaoVictorVM/comporta/internal/keys"
	"github.com/JoaoVictorVM/comporta/internal/limits"
	"github.com/JoaoVictorVM/comporta/internal/metering"
	"github.com/JoaoVictorVM/comporta/internal/pricing"
	"github.com/JoaoVictorVM/comporta/internal/proxy"
	"github.com/JoaoVictorVM/comporta/internal/server"
	"github.com/JoaoVictorVM/comporta/internal/storage"
	"github.com/JoaoVictorVM/comporta/internal/storage/sqlite"
	"github.com/JoaoVictorVM/comporta/internal/useridentity"
)

const providerSecret = "provider-key-for-tests"

type resultCollector struct {
	mu      sync.Mutex
	results []proxy.AttemptResult
}

func (c *resultCollector) Publish(_ context.Context, result proxy.AttemptResult) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.results = append(c.results, result)
	return nil
}

func (c *resultCollector) last(t testing.TB) proxy.AttemptResult {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		count := len(c.results)
		var result proxy.AttemptResult
		if count > 0 {
			result = c.results[count-1]
		}
		c.mu.Unlock()
		if count > 0 {
			return result
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no attempt result")
	return proxy.AttemptResult{}
}

func (c *resultCollector) waitCount(t testing.TB, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		count := len(c.results)
		c.mu.Unlock()
		if count >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("attempt completion barrier not reached")
}

// gateway is a complete service: managers, both listeners, a fake upstream.
type gateway struct {
	usage      *sqlite.Store
	accounting *metering.Service
	limits     *limits.Manager
	userHeader string
	handler    *proxy.Handler
	t          testing.TB
	path       string
	dir        string
	config     *config.Manager
	store      *keys.Store
	keyManager *keys.Manager
	runtime    *server.Runtime
	identity   *useridentity.Service
	results    *resultCollector
	upstream   *httptest.Server
	requests   chan fakeprovider.Request
	baseYAML   string
}

func freePort(t testing.TB) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	return listener.Addr().(*net.TCPAddr).Port
}

func configText(upstreamURL string, proxyPort, adminPort int, extraListeners string) string {
	return fmt.Sprintf(`timezone: UTC
providers:
  anthropic:
    api_format: anthropic
    base_url: %[1]s
    api_key: ${COMPORTA_INTEGRATION_KEY}
    models: [fake-claude]
  openai:
    api_format: openai
    base_url: %[1]s/v1
    api_key: ${COMPORTA_INTEGRATION_KEY}
    models: [fake-gpt]
apps:
  bot:
    allowed_models: [fake-claude, fake-gpt]
  other:
    allowed_models: [fake-gpt]
listeners:
  proxy: {port: %[2]d}
  admin: {port: %[3]d}
%[4]s`, upstreamURL, proxyPort, adminPort, extraListeners)
}

func startGateway(t testing.TB, fake fakeprovider.Options, extraListeners string) *gateway {
	t.Helper()
	return startGatewayWithHooks(t, fake, extraListeners, proxy.Hooks{})
}

func startGatewayWithHooks(t testing.TB, fake fakeprovider.Options, extraListeners string, hooks proxy.Hooks) *gateway {
	return startGatewayOptions(t, fake, extraListeners, hooks, false)
}

func startMeteredGateway(t testing.TB, fake fakeprovider.Options) *gateway {
	return startGatewayOptions(t, fake, "", proxy.Hooks{}, true)
}

func startGatewayOptions(t testing.TB, fake fakeprovider.Options, extraListeners string, hooks proxy.Hooks, metered bool) *gateway {
	t.Helper()
	return startConfiguredGateway(t, fake, gatewayOptions{extraListeners: extraListeners, hooks: hooks, metered: metered})
}

// gatewayOptions adjusts a test gateway. edit changes the initial YAML; wrap
// replaces the store the limit coordinator persists through, to inject
// failures into limits alone while the usage journal stays healthy.
type gatewayOptions struct {
	extraListeners string
	hooks          proxy.Hooks
	metered        bool
	edit           func(string) string
	wrap           func(*sqlite.Store) storage.LimitStore
	upstream       func(http.Handler) http.Handler
	// prepare runs on the data directory before the usage store opens.
	prepare func(dataDir string)
}

func startConfiguredGateway(t testing.TB, fake fakeprovider.Options, options gatewayOptions) *gateway {
	t.Helper()
	extraListeners, hooks, metered := options.extraListeners, options.hooks, options.metered
	t.Setenv("COMPORTA_INTEGRATION_KEY", providerSecret)
	requests := make(chan fakeprovider.Request, 16)
	previous := fake.OnRequest
	fake.OnRequest = func(request fakeprovider.Request) {
		select {
		case requests <- request:
		default:
		}
		if previous != nil {
			previous(request)
		}
	}
	provider := fakeprovider.New(fake)
	if options.upstream != nil {
		provider = options.upstream(provider)
	}
	upstream := httptest.NewServer(provider)
	t.Cleanup(upstream.Close)

	dir := t.TempDir()
	path := filepath.Join(dir, "comporta.yaml")
	proxyPort, adminPort := freePort(t), freePort(t)
	for adminPort == proxyPort {
		adminPort = freePort(t)
	}
	text := configText(upstream.URL, proxyPort, adminPort, extraListeners)
	if metered {
		text = strings.Replace(text, "apps:\n", "pricing:\n  fake-claude: {input: 2, output: 6, cache_write: 2.5, cache_read: 0.2}\n  fake-gpt: {input: 2, output: 6, cache_write: 2.5, cache_read: 0.2}\napps:\n", 1)
	}
	if options.edit != nil {
		text = options.edit(text)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := config.NewManager(config.ManagerOptions{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	store := keys.NewStore(manager.Capture().DataDir())
	if _, err := store.Create(context.Background(), "other"); err != nil {
		t.Fatal(err)
	}
	keyManager, err := keys.NewManager(keys.ManagerOptions{Dir: manager.Capture().DataDir()})
	if err != nil {
		t.Fatal(err)
	}
	identity, err := useridentity.Open(context.Background(), useridentity.Options{Dir: manager.Capture().DataDir()})
	if err != nil {
		t.Fatal(err)
	}
	results := &resultCollector{}
	var usage *sqlite.Store
	var accounting *metering.Service
	var coordinator *limits.Manager
	var accountant proxy.Accounting
	workers := []server.Worker{manager.Run, keyManager.Run}
	if metered {
		if options.prepare != nil {
			options.prepare(manager.Capture().DataDir())
		}
		usage, err = sqlite.Open(context.Background(), manager.Capture().DataDir())
		if err != nil {
			t.Fatal(err)
		}
		catalog, err := pricing.Embedded()
		if err != nil {
			t.Fatal(err)
		}
		accounting = metering.New(usage, catalog, slog.New(slog.DiscardHandler), func() int64 { return manager.Capture().Storage().RetentionDays })
		var limitStore storage.LimitStore = usage
		if options.wrap != nil {
			limitStore = options.wrap(usage)
		}
		coordinator = limits.New(limits.Options{Store: limitStore, Publisher: usage, Logger: slog.New(slog.DiscardHandler)})
		if err = coordinator.Recover(context.Background(), manager.Capture()); err != nil {
			t.Fatal(err)
		}
		if err = accounting.UseLimits(coordinator, limitStore, manager); err != nil {
			t.Fatal(err)
		}
		accountant = accounting
		workers = append(workers, accounting.Run, coordinator.Run)
	}
	authHTTP := auth.NewHTTP(keyManager, auth.NewAuthenticator(keyManager, nil), auth.NewGuard(nil, nil))
	handler := proxy.NewHandler(proxy.Options{Auth: authHTTP, Sink: results, Identity: identity, Hooks: hooks, Accounting: accountant})
	runtime, err := server.Open(context.Background(), server.Options{
		Source:  manager,
		Proxy:   server.ProxySurface(manager, handler),
		Admin:   server.AdminSurface(manager),
		Workers: workers,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runtime.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			t.Error("the gateway did not stop")
		}
		handler.Close()
		if usage != nil {
			usage.Close()
		}
	})
	return &gateway{
		usage: usage, accounting: accounting, limits: coordinator,
		handler: handler, t: t, path: path, dir: dir, config: manager, store: store, keyManager: keyManager,
		runtime: runtime, identity: identity, results: results, upstream: upstream, requests: requests, baseYAML: text,
	}
}

// createKey makes a key and waits until the running service accepts it.
func (g *gateway) createKey(app string) keys.Created {
	g.t.Helper()
	created, err := g.store.Create(context.Background(), app)
	if err != nil {
		g.t.Fatal(err)
	}
	// Asking the manager and not the listener: each refused attempt counts against the
	// address, and a slow reload under load would trip the guard before the key arrives.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for entry := range g.keyManager.Capture().Entries() {
			if entry.Identity.KeyID() == created.Metadata.ID {
				return created
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	g.t.Fatal("the key was not picked up")
	return created
}

func (g *gateway) proxyURL(path string) string {
	return "http://" + g.runtime.ProxyAddr().String() + path
}

func (g *gateway) reload(edit func(string) string) {
	g.t.Helper()
	g.baseYAML = edit(g.baseYAML)
	if err := os.WriteFile(g.path, []byte(g.baseYAML), 0o600); err != nil {
		g.t.Fatal(err)
	}
	if status := g.config.Reload(context.Background()); status.Result != config.ResultApplied {
		g.t.Fatalf("reload: %+v", status)
	}
}

func (g *gateway) call(path, key, body string) (*http.Response, string) {
	g.t.Helper()
	request, err := http.NewRequest(http.MethodPost, g.proxyURL(path), strings.NewReader(body))
	if err != nil {
		g.t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	if strings.HasPrefix(path, "/v1/messages") {
		request.Header.Set("X-Api-Key", key)
		request.Header.Set("Anthropic-Version", "2023-06-01")
	} else {
		request.Header.Set("Authorization", "Bearer "+key)
	}
	if g.userHeader != "" {
		request.Header.Set("X-Comporta-User", g.userHeader)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		g.t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		g.t.Fatal(err)
	}
	return response, string(data)
}

func (g *gateway) lastRequest() fakeprovider.Request {
	g.t.Helper()
	select {
	case request := <-g.requests:
		return request
	case <-time.After(3 * time.Second):
		g.t.Fatal("the provider received nothing")
		return fakeprovider.Request{}
	}
}
