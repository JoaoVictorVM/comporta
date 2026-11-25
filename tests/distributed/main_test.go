//go:build distributed

// Package distributed runs real "comporta serve" processes against shared
// Postgres and Redis containers, so replicas share nothing in memory.
package distributed

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/JoaoVictorVM/comporta/internal/fakeprovider"
	"github.com/JoaoVictorVM/comporta/internal/testkit"
)

var binary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "comporta-bin")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	binary = filepath.Join(dir, "comporta")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	build := exec.Command("go", "build", "-o", binary, "../../cmd/comporta")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintln(os.Stderr, string(out))
		os.Exit(1)
	}
	code := testkit.RunWithDatabases(m)
	os.RemoveAll(dir)
	os.Exit(code)
}

const providerSecret = "provider-key-for-tests"

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

// cluster is the shared configuration of the replicas of one test.
type cluster struct {
	t        *testing.T
	dir      string
	dsn      string
	env      []string
	upstream *httptest.Server
	key      string
	token    string
	replicas []*replica
}

type replica struct {
	path      string
	proxyPort int
	adminPort int
	cmd       *exec.Cmd
	stderr    *syncBuffer
}

func freePort(t testing.TB) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func writeKey(t testing.TB, path string) {
	raw := make([]byte, 32)
	_, _ = rand.Read(raw)
	if err := os.WriteFile(path, raw, 0o400); err != nil {
		t.Fatal(err)
	}
}

// newCluster prepares the shared secrets, the stores and the first key and
// admin token, then starts n replicas.
func newCluster(t *testing.T, n int, apps string) *cluster {
	t.Helper()
	c := &cluster{t: t, dir: t.TempDir(), dsn: testkit.Postgres(t)}
	addr, db := testkit.Redis(t)
	// The configuration accepts databases 0 to 15 only. Clusters never run in
	// parallel and each one flushes its database below, so reusing an index
	// after sixteen clusters is safe.
	db %= 16
	c.upstream = httptest.NewServer(fakeprovider.New(fakeprovider.Options{}))
	t.Cleanup(c.upstream.Close)
	secrets := filepath.Join(c.dir, "secrets")
	if err := os.MkdirAll(secrets, 0o700); err != nil {
		t.Fatal(err)
	}
	writeKey(t, filepath.Join(secrets, "hmac.key"))
	writeKey(t, filepath.Join(secrets, "cursor.key"))
	clusterID := fmt.Sprintf("it-%d", db)
	flush := exec.Command("docker", "exec", testkit.RedisContainer(), "redis-cli", "-n", fmt.Sprint(db), "FLUSHDB")
	_ = flush.Run()
	c.env = append(os.Environ(), "COMPORTA_INTEGRATION_KEY="+providerSecret, "COMPORTA_PG_DSN="+c.dsn)
	for i := range n {
		r := &replica{proxyPort: freePort(t), adminPort: freePort(t)}
		r.path = filepath.Join(c.dir, fmt.Sprintf("replica-%d.yaml", i))
		text := fmt.Sprintf(`timezone: UTC
data_dir: ./data-%[7]d
providers:
  anthropic:
    api_format: anthropic
    base_url: %[1]s
    api_key: ${COMPORTA_INTEGRATION_KEY}
    models: [fake-claude]
  openai:
    api_format: openai
    base_url: %[1]s
    api_key: ${COMPORTA_INTEGRATION_KEY}
    models: [fake-gpt]
pricing:
  fake-claude: {input: 2, output: 6, cache_write: 2.5, cache_read: 0.2}
  fake-gpt: {input: 2, output: 6, cache_write: 2.5, cache_read: 0.2}
%[6]s
listeners:
  proxy: {port: %[2]d}
  admin: {port: %[3]d}
deployment:
  mode: distributed
  replicas: %[8]d
  cluster_id: %[4]s
  config_revision: 1
  hmac_key_file: secrets/hmac.key
  cursor_key_file: secrets/cursor.key
storage:
  backend: postgres
  postgres:
    dsn: ${COMPORTA_PG_DSN}
  redis:
    address: %[5]s
    database: %[9]d
    tls: {enabled: false}
`, c.upstream.URL, r.proxyPort, r.adminPort, clusterID, addr, apps, i, n, db)
		if err := os.WriteFile(r.path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		c.replicas = append(c.replicas, r)
	}
	first := c.replicas[0].path
	c.run("storage", "migrate", "--config", first)
	token := regexp.MustCompile(`store it now\): ([0-9a-f]{64})`).FindStringSubmatch(c.run("admin", "reset-token", "--config", first))
	if token == nil {
		t.Fatal("no admin token")
	}
	c.token = token[1]
	key := regexp.MustCompile(`(cmp_[A-Za-z0-9]+)`).FindStringSubmatch(c.run("keys", "create", "bot", "--config", first))
	if key == nil {
		t.Fatal("no app key")
	}
	c.key = key[1]
	for _, r := range c.replicas {
		c.start(r)
	}
	return c
}

func (c *cluster) run(args ...string) string {
	c.t.Helper()
	cmd := exec.Command(binary, args...)
	cmd.Env, cmd.Dir = c.env, c.dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		c.t.Fatalf("comporta %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func (c *cluster) start(r *replica) {
	c.t.Helper()
	r.stderr = &syncBuffer{}
	r.cmd = exec.Command(binary, "serve", "--config", r.path)
	r.cmd.Env, r.cmd.Dir, r.cmd.Stderr, r.cmd.Stdout = c.env, c.dir, r.stderr, io.Discard
	if err := r.cmd.Start(); err != nil {
		c.t.Fatal(err)
	}
	c.t.Cleanup(func() { c.kill(r) })
	deadline := time.Now().Add(60 * time.Second)
	for !c.ready(r) {
		if time.Now().After(deadline) || (r.cmd.ProcessState != nil) {
			c.t.Fatalf("replica did not become ready: %s", r.stderr.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (c *cluster) kill(r *replica) {
	if r.cmd != nil && r.cmd.Process != nil {
		_ = r.cmd.Process.Kill()
		_, _ = r.cmd.Process.Wait()
		r.cmd = nil
	}
}

func (c *cluster) ready(r *replica) bool {
	response, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/readyz", r.adminPort))
	if err != nil {
		return false
	}
	defer response.Body.Close()
	return response.StatusCode == http.StatusOK
}

// call sends one generation and returns the status and the error code.
func (c *cluster) call(r *replica, user string) (int, string) {
	content, _ := json.Marshal(testkit.Prompt("hello"))
	body := `{"model":"fake-claude","max_tokens":16,"messages":[{"role":"user","content":` + string(content) + `}]}`
	request, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/v1/messages", r.proxyPort), strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Api-Key", c.key)
	request.Header.Set("Anthropic-Version", "2023-06-01")
	if user != "" {
		request.Header.Set("X-Comporta-User", user)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return 0, err.Error()
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(response.Body)
	code := ""
	if m := regexp.MustCompile(`"type":"([a-z_]+)"`).FindAllStringSubmatch(string(data), -1); len(m) > 1 {
		code = m[1][1]
	}
	return response.StatusCode, code
}

func (c *cluster) admin(r *replica, method, path, body string) (int, string) {
	request, _ := http.NewRequest(method, fmt.Sprintf("http://127.0.0.1:%d%s", r.adminPort, path), strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return 0, err.Error()
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(response.Body)
	return response.StatusCode, string(data)
}

func (c *cluster) db() *sql.DB {
	db, err := sql.Open("pgx", c.dsn)
	if err != nil {
		c.t.Fatal(err)
	}
	c.t.Cleanup(func() { db.Close() })
	return db
}

func withDeadline(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}

func runBinary(args ...string) (string, error) {
	out, err := exec.Command(binary, args...).CombinedOutput()
	return string(out), err
}
