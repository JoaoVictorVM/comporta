package proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/JoaoVictorVM/comporta/internal/auth"
	"github.com/JoaoVictorVM/comporta/internal/config"
)

func TestHeadersAllowlistAndProviderCredential(t *testing.T) {
	incoming := http.Header{
		"Content-Type":      {"application/json"},
		"Anthropic-Version": {"2023-06-01"},
		"Anthropic-Beta":    {"a", "b"},
		"X-Api-Key":         {"cmp_client"},
		"Authorization":     {"Bearer cmp_client"},
		"Cookie":            {"session=1"},
		"X-Comporta-User":   {"user"},
		"X-Stainless-Lang":  {"python"},
		"X-Forwarded-For":   {"1.1.1.1"},
		"Host":              {"evil.example"},
	}
	target, _ := url.Parse("http://provider.example/v1/messages")
	secret := secretFor(t, "provider-key")

	request, err := newUpstreamRequest(context.Background(), target, incoming, auth.FormatAnthropic, secret, true, false, []byte("{}"))
	if err != nil {
		t.Fatal(err)
	}
	if got := request.Header.Values("Anthropic-Beta"); len(got) != 2 {
		t.Fatalf("beta values %v", got)
	}
	if request.Header.Get("X-Api-Key") != "provider-key" || request.Header.Get("Authorization") != "" {
		t.Fatalf("credentials %v", request.Header)
	}
	if request.Header.Get("Accept-Encoding") != "identity" || request.Header.Get("Accept") != "application/json" {
		t.Fatalf("accept %v", request.Header)
	}
	for _, name := range []string{"Cookie", "X-Comporta-User", "X-Stainless-Lang", "X-Forwarded-For", "Host"} {
		if len(request.Header.Values(name)) != 0 {
			t.Errorf("%s was forwarded", name)
		}
	}
	if request.Host != "provider.example" || request.GetBody != nil || request.ContentLength != 2 {
		t.Fatalf("host %q, GetBody set %v, length %d", request.Host, request.GetBody != nil, request.ContentLength)
	}

	openAI, err := newUpstreamRequest(context.Background(), target, incoming, auth.FormatOpenAI, secret, true, true, []byte("{}"))
	if err != nil {
		t.Fatal(err)
	}
	if openAI.Header.Get("Authorization") != "Bearer provider-key" || openAI.Header.Get("X-Api-Key") != "" {
		t.Fatalf("credentials %v", openAI.Header)
	}
	if openAI.Header.Get("Anthropic-Version") != "" {
		t.Fatal("anthropic headers belong to the anthropic route only")
	}
	if openAI.Header.Get("Accept") != "text/event-stream" {
		t.Fatal("stream accept missing")
	}

	local, err := newUpstreamRequest(context.Background(), target, incoming, auth.FormatOpenAI, config.Secret{}, false, false, []byte("{}"))
	if err != nil {
		t.Fatal(err)
	}
	if local.Header.Get("Authorization") != "" {
		t.Fatal("a local provider without a secret gets no credential")
	}
}

func secretFor(t *testing.T, value string) config.Secret {
	t.Helper()
	t.Setenv("COMPORTA_SECRET_HELPER", value)
	manager := managerFor(t, "timezone: UTC\nproviders:\n  p:\n    api_format: openai\n    base_url: https://x.example\n    api_key: ${COMPORTA_SECRET_HELPER}\n    models: [m]\n")
	secret, ok := manager.Capture().ProviderAPIKey("p")
	if !ok {
		t.Fatal("secret missing")
	}
	return secret
}

func managerFor(t *testing.T, content string) *config.Manager {
	t.Helper()
	path := filepath.Join(t.TempDir(), "comporta.yaml")
	writeFile(t, path, content)
	manager, err := config.NewManager(config.ManagerOptions{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

func TestConnectionNamedHeadersDropped(t *testing.T) {
	incoming := http.Header{
		"Connection":        {"close, Anthropic-Beta", "Anthropic-Version"},
		"Anthropic-Beta":    {"a"},
		"Anthropic-Version": {"2023-06-01"},
		"Content-Type":      {"application/json"},
	}
	target, _ := url.Parse("http://provider.example/v1/messages")
	request, err := newUpstreamRequest(context.Background(), target, incoming, auth.FormatAnthropic, config.Secret{}, false, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if request.Header.Get("Anthropic-Beta") != "" || request.Header.Get("Anthropic-Version") != "" || request.Header.Get("Connection") != "" {
		t.Fatalf("named headers forwarded: %v", request.Header)
	}
	if request.Header.Get("Content-Type") == "" {
		t.Fatal("content type lost")
	}
}

func TestConfiguredBasePathJoinsV1Once(t *testing.T) {
	cases := map[string]string{
		"https://api.openai.com/v1":        "https://api.openai.com/v1/chat/completions",
		"https://api.openai.com/v1/":       "https://api.openai.com/v1/chat/completions",
		"http://localhost:11434":           "http://localhost:11434/v1/chat/completions",
		"http://localhost:11434/":          "http://localhost:11434/v1/chat/completions",
		"https://x.example/gateway/v1":     "https://x.example/gateway/v1/chat/completions",
		"https://x.example/gateway":        "https://x.example/gateway/v1/chat/completions",
		"https://x.example/v1/v1":          "https://x.example/v1/v1/chat/completions",
		"https://x.example/caf%C3%A9/base": "https://x.example/caf%C3%A9/base/v1/chat/completions",
	}
	for base, want := range cases {
		got, err := upstreamURL(base, RouteChatCompletions)
		if err != nil {
			t.Fatalf("%s: %v", base, err)
		}
		if got.String() != want {
			t.Errorf("%s: got %s", base, got)
		}
	}
}

func TestAllowedAddress(t *testing.T) {
	cases := []struct {
		addr, host string
		local      bool
		want       bool
	}{
		{"127.0.0.1", "localhost", true, true},
		{"::1", "localhost", true, true},
		{"10.0.0.1", "localhost", true, false},
		{"8.8.8.8", "localhost", true, false},
		{"192.168.1.10", "192.168.1.10", true, true},
		{"10.0.0.1", "10.0.0.1", true, true},
		{"8.8.8.8", "api.example", false, true},
		{"2001:4860:4860::8888", "api.example", false, true},
		{"127.0.0.1", "api.example", false, false},
		{"::ffff:127.0.0.1", "api.example", false, false},
		{"10.0.0.1", "api.example", false, false},
		{"172.16.0.1", "api.example", false, false},
		{"fc00::1", "api.example", false, false},
		{"169.254.169.254", "api.example", false, false},
		{"fe80::1", "api.example", false, false},
		{"224.0.0.1", "api.example", false, false},
		{"0.0.0.0", "api.example", false, false},
		{"::", "api.example", false, false},
		{"169.254.169.254", "169.254.169.254", false, false},
	}
	for _, tc := range cases {
		if got := allowedAddress(netip.MustParseAddr(tc.addr), tc.host, tc.local); got != tc.want {
			t.Errorf("%s via %s (local %v): got %v", tc.addr, tc.host, tc.local, got)
		}
	}
}

type staticResolver struct {
	addrs []netip.Addr
	calls int
}

func (r *staticResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	r.calls++
	return r.addrs, nil
}

func TestResolvedAddressGuard(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)

	t.Run("every answer must be allowed", func(t *testing.T) {
		resolver := &staticResolver{addrs: []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("127.0.0.1")}}
		dialer := &guardedDialer{resolver: resolver}
		if _, err := dialer.DialContext(context.Background(), "tcp", "api.example:"+port); !errors.Is(err, errAddressRefused) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("a remote name pointing at loopback is refused", func(t *testing.T) {
		resolver := &staticResolver{addrs: []netip.Addr{netip.MustParseAddr("127.0.0.1")}}
		dialer := &guardedDialer{resolver: resolver}
		if _, err := dialer.DialContext(context.Background(), "tcp", "api.example:"+port); !errors.Is(err, errAddressRefused) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("localhost connects to the resolved address, with one lookup", func(t *testing.T) {
		resolver := &staticResolver{addrs: []netip.Addr{netip.MustParseAddr("127.0.0.1")}}
		dialer := &guardedDialer{local: true, resolver: resolver}
		conn, err := dialer.DialContext(context.Background(), "tcp", "localhost:"+port)
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.Close()
		if resolver.calls != 1 {
			t.Fatalf("lookups: %d", resolver.calls)
		}
	})
	t.Run("a literal skips resolution", func(t *testing.T) {
		resolver := &staticResolver{}
		dialer := &guardedDialer{local: true, resolver: resolver}
		conn, err := dialer.DialContext(context.Background(), "tcp", "127.0.0.1:"+port)
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.Close()
		if resolver.calls != 0 {
			t.Fatal("a literal was resolved")
		}
	})
}

func poolSnapshot(t *testing.T, tlsLine string) (*config.Snapshot, config.Provider) {
	t.Helper()
	snapshot := managerFor(t, "timezone: UTC\nproviders:\n  p:\n    api_format: openai\n    base_url: https://127.0.0.1:1\n"+tlsLine+"    models: [m]\n").Capture()
	provider, _ := snapshot.Provider("p")
	return snapshot, provider
}

func TestEnvironmentProxyIgnored(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	snapshot, provider := poolSnapshot(t, "")
	pool := NewTransportPool(nil, nil)
	transport, release := pool.acquire(snapshot, "p", provider)
	defer release()
	if transport.Proxy != nil {
		t.Fatal("the transport must not consult proxy settings")
	}
	if !transport.DisableCompression {
		t.Fatal("compression must stay off so bytes are forwarded as received")
	}
}

func TestTLSMinimumAndVerification(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	server.TLS = &tls.Config{MaxVersion: tls.VersionTLS12}
	server.StartTLS()
	defer server.Close()
	target, _ := url.Parse(server.URL)

	roundTrip := func(pool *TransportPool, tlsLine string) error {
		snapshot, provider := poolSnapshot(t, tlsLine)
		transport, release := pool.acquire(snapshot, "p", provider)
		defer release()
		defer transport.CloseIdleConnections()
		request, err := http.NewRequest(http.MethodGet, target.String(), nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := transport.RoundTrip(request)
		if err == nil {
			_ = response.Body.Close()
		}
		return err
	}

	if err := roundTrip(NewTransportPool(nil, nil), ""); err == nil {
		t.Fatal("an untrusted certificate was accepted")
	}
	roots := server.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	if err := roundTrip(NewTransportPool(nil, roots), ""); err != nil {
		t.Fatalf("a trusted certificate was refused: %v", err)
	}
	if err := roundTrip(NewTransportPool(nil, roots), "    tls_min_version: \"1.3\"\n"); err == nil {
		t.Fatal("TLS 1.2 was accepted with a 1.3 floor")
	}
}

func TestPoolRetiresOldGenerations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "comporta.yaml")
	base := "timezone: UTC\nproviders:\n  p:\n    api_format: openai\n    base_url: http://127.0.0.1:9\n    models: [m]\n"
	writeFile(t, path, base)
	manager, err := config.NewManager(config.ManagerOptions{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	pool := NewTransportPool(nil, nil)
	entries := func() int {
		pool.mu.Lock()
		defer pool.mu.Unlock()
		return len(pool.entries)
	}
	oldSnapshot := manager.Capture()
	provider, _ := oldSnapshot.Provider("p")
	oldTransport, releaseOld := pool.acquire(oldSnapshot, "p", provider)
	same, releaseSame := pool.acquire(oldSnapshot, "p", provider)
	if same != oldTransport {
		t.Fatal("one generation must share a transport")
	}
	releaseSame()

	writeFile(t, path, strings.Replace(base, "timezone: UTC", "timezone: America/Sao_Paulo", 1))
	if status := manager.Reload(context.Background()); status.Result != config.ResultApplied {
		t.Fatalf("reload: %+v", status)
	}
	newSnapshot := manager.Capture()
	newTransport, releaseNew := pool.acquire(newSnapshot, "p", provider)
	defer releaseNew()
	if newTransport == oldTransport {
		t.Fatal("a new generation must get its own transport")
	}
	if entries() != 2 {
		t.Fatalf("an operation in flight lost its transport: %d entries", entries())
	}
	releaseOld()
	releaseOld()
	if entries() != 1 {
		t.Fatalf("the retired transport was kept: %d entries", entries())
	}
}

func TestClassifyTransportError(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		connected bool
		code      Code
		status    int
		message   string
	}{
		{"refused", errors.New("refused"), false, CodeProviderUnreachable, 502, "provider p unreachable"},
		{"connect timeout", context.DeadlineExceeded, false, CodeProviderUnreachable, 502, "provider p unreachable (connect timeout after 10s)"},
		{"header timeout", timeoutError{}, true, CodeProviderTimeout, 504, "provider p timed out"},
		{"reset", errors.New("reset"), true, CodeProviderUnreachable, 502, "provider p unreachable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyTransportError(tc.err, tc.connected, "p")
			if got.Code != tc.code || got.Status != tc.status || got.Message != tc.message {
				t.Fatalf("got %+v", got)
			}
			if strings.Contains(got.Message, tc.err.Error()) && tc.err != context.DeadlineExceeded {
				t.Fatalf("raw error exposed: %s", got.Message)
			}
		})
	}
}

type timeoutError struct{}

func (timeoutError) Error() string   { return "secret-bearing raw text" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

var _ net.Error = timeoutError{}
