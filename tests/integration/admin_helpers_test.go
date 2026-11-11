package integration

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/cli"
	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/fakeprovider"
	"github.com/JoaoVictorVM/comporta/internal/keys"
)

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// served is the real "comporta serve" command against a fake provider, with
// its ports and data directory kept across restarts.
type served struct {
	t         testing.TB
	path      string
	dataDir   string
	proxyPort int
	adminPort int
	token     string
	keys      map[string]string
	stdout    *syncBuffer
	stderr    *syncBuffer
	stop      func()
}

var tokenLine = regexp.MustCompile(`store it now\): ([0-9a-f]{64})`)

func startServed(t testing.TB, fake fakeprovider.Options) *served {
	t.Helper()
	t.Setenv("COMPORTA_INTEGRATION_KEY", providerSecret)
	upstream := httptest.NewServer(fakeprovider.New(fake))
	t.Cleanup(upstream.Close)
	dir := t.TempDir()
	path := filepath.Join(dir, "comporta.yaml")
	proxyPort, adminPort := freePort(t), freePort(t)
	for adminPort == proxyPort {
		adminPort = freePort(t)
	}
	text := configText(upstream.URL, proxyPort, adminPort, "")
	text = strings.Replace(text, "apps:\n", "pricing:\n  fake-claude: {input: 2, output: 6, cache_write: 2.5, cache_read: 0.2}\n  fake-gpt: {input: 2, output: 6, cache_write: 2.5, cache_read: 0.2}\napps:\n", 1)
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	candidate, err := config.Load(path, config.Options{})
	if err != nil {
		t.Fatal(err)
	}
	s := &served{t: t, path: path, dataDir: candidate.DataDir(), proxyPort: proxyPort, adminPort: adminPort, keys: map[string]string{}}
	store := keys.NewStore(s.dataDir)
	for _, app := range []string{"bot", "other"} {
		created, err := store.Create(context.Background(), app)
		if err != nil {
			t.Fatal(err)
		}
		s.keys[app] = created.Key.Reveal()
	}
	s.start()
	t.Cleanup(s.shutdown)
	return s
}

func (s *served) start() {
	s.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	s.stdout, s.stderr = &syncBuffer{}, &syncBuffer{}
	exit := make(chan int, 1)
	go func() { exit <- cli.Execute(ctx, []string{"serve", "--config", s.path}, s.stdout, s.stderr) }()
	s.stop = func() {
		cancel()
		select {
		case code := <-exit:
			if code != 0 {
				s.t.Errorf("serve exited %d: %s", code, s.stderr.String())
			}
		case <-time.After(30 * time.Second):
			s.t.Error("serve did not stop")
		}
	}
	deadline := time.Now().Add(20 * time.Second)
	for !strings.Contains(s.stderr.String(), `"msg":"listening"`) {
		select {
		case code := <-exit:
			s.t.Fatalf("serve exited %d: %s", code, s.stderr.String())
		default:
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("serve did not start: %s", s.stderr.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if match := tokenLine.FindStringSubmatch(s.stdout.String()); match != nil {
		s.token = match[1]
	}
	if s.token == "" {
		s.t.Fatal("no admin token was shown on the first start")
	}
}

func (s *served) shutdown() {
	if s.stop != nil {
		s.stop()
		s.stop = nil
	}
}

func (s *served) restart() {
	s.t.Helper()
	s.shutdown()
	before := s.token
	s.start()
	if strings.Contains(s.stdout.String(), "store it now") {
		s.t.Fatal("a restart showed the admin token again")
	}
	s.token = before
}

func (s *served) adminURL(path string) string {
	return "http://127.0.0.1:" + strconv.Itoa(s.adminPort) + path
}

func (s *served) proxyURL(path string) string {
	return "http://127.0.0.1:" + strconv.Itoa(s.proxyPort) + path
}

// admin calls the admin API with the bearer token.
func (s *served) admin(method, path, body string) (int, string) {
	s.t.Helper()
	request, err := http.NewRequest(method, s.adminURL(path), strings.NewReader(body))
	if err != nil {
		s.t.Fatal(err)
	}
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set("Authorization", "Bearer "+s.token)
	return send(s.t, request)
}

// call sends a chat completion through the proxy for an app and a user.
func (s *served) call(app, user, body string) (int, string) {
	s.t.Helper()
	request, err := http.NewRequest(http.MethodPost, s.proxyURL("/v1/chat/completions"), strings.NewReader(body))
	if err != nil {
		s.t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+s.keys[app])
	if user != "" {
		request.Header.Set("X-Comporta-User", user)
	}
	return send(s.t, request)
}

func send(t testing.TB, request *http.Request) (int, string) {
	t.Helper()
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, string(data)
}
