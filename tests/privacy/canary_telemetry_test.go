package privacy

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	otlpcollector "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/JoaoVictorVM/comporta/internal/testkit"
)

// otlpReceiver is a local OTLP/HTTP collector that keeps the raw bodies it
// receives, so the scan sees exactly what crossed the network.
type otlpReceiver struct {
	server *httptest.Server
	mu     sync.Mutex
	bodies [][]byte
	roots  int
}

func newOTLPReceiver(t *testing.T) *otlpReceiver {
	t.Helper()
	r := &otlpReceiver{}
	r.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		decoded := &otlpcollector.ExportTraceServiceRequest{}
		if err := proto.Unmarshal(body, decoded); err != nil {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		roots := 0
		for _, rs := range decoded.ResourceSpans {
			for _, ss := range rs.ScopeSpans {
				for _, s := range ss.Spans {
					if s.Name == "comporta.request" {
						roots++
					}
				}
			}
		}
		r.mu.Lock()
		r.bodies = append(r.bodies, body)
		r.roots += roots
		r.mu.Unlock()
		w.Header().Set("Content-Type", "application/x-protobuf")
	}))
	t.Cleanup(r.server.Close)
	return r
}

func (r *otlpReceiver) waitRoots(t *testing.T, want int) [][]byte {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		r.mu.Lock()
		roots, bodies := r.roots, append([][]byte(nil), r.bodies...)
		r.mu.Unlock()
		if roots >= want {
			return bodies
		}
		if time.Now().After(deadline) {
			t.Fatalf("the collector received %d of %d traces", roots, want)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (g *liveGateway) adminToken(t *testing.T) string {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		data, _ := os.ReadFile(g.logs[2])
		if match := regexp.MustCompile(`store it now\): ([0-9a-f]{64})`).FindSubmatch(data); match != nil {
			return string(match[1])
		}
	}
	t.Fatal("no admin token printed")
	return ""
}

// sendWith makes a request with extra headers chosen by the client.
func (g *liveGateway) sendWith(name, path, key, body string, header map[string]string) int {
	g.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	request := newCall(g.t, ctx, g.url(path), key, "", body)
	for k, v := range header {
		request.Header.Set(k, v)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return 0
	}
	defer func() { _ = response.Body.Close() }()
	data, _ := io.ReadAll(response.Body)
	run.scanner.Scan("errors", name, data)
	return response.StatusCode
}

// TestCanaryAbsentFromMetricsTracesAndLogs sends marked prompts, a marked
// model name, marked identifiers, client tracing headers, an upstream error
// and a stream through the real binary with metrics and tracing on, then
// scans the scrape, the OTLP bodies received by a local collector and every
// log. The user hashes the gateway stored must not appear either.
func TestCanaryAbsentFromMetricsTracesAndLogs(t *testing.T) {
	run.markCase("process/telemetry")
	receiver := newOTLPReceiver(t)
	g := startLiveFake(t, []string{"--scenario", "success", "--model-scenario", "fake-gpt=overload"}, "", func(text string) string {
		return text + "telemetry:\n  otel_enabled: true\n  otel_endpoint: " + receiver.server.URL + "\n"
	})
	defer g.stop()
	token := g.adminToken(t)
	user := testkit.UserMarker()
	marker := testkit.Marker()
	run.markConstructor("AnthropicBody")
	run.markConstructor("AnthropicStreamBody")
	run.markConstructor("OpenAIBody")
	run.markConstructor("CountBody")
	sent := 0
	expect := func(name string, got, want int) {
		t.Helper()
		sent++
		if got != want {
			t.Fatalf("%s: status %d, want %d", name, got, want)
		}
	}
	expect("telemetry-json", g.send("telemetry-json", "/v1/messages", g.key, user, testkit.AnthropicBody()), http.StatusOK)
	expect("telemetry-stream", g.send("telemetry-stream", "/v1/messages", g.key, user, testkit.AnthropicStreamBody()), http.StatusOK)
	expect("telemetry-count", g.send("telemetry-count", "/v1/messages/count_tokens", g.key, user, testkit.CountBody()), http.StatusOK)
	expect("telemetry-upstream-error", g.send("telemetry-upstream-error", "/v1/chat/completions", g.key, user, testkit.OpenAIBody()), 529)
	unknownModel := strings.Replace(testkit.AnthropicBody(), `"model":"fake-claude"`, `"model":"`+marker+`"`, 1)
	expect("telemetry-unknown-model", g.send("telemetry-unknown-model", "/v1/messages", g.key, user, unknownModel), http.StatusBadRequest)
	expect("telemetry-client-headers", g.sendWith("telemetry-client-headers", "/v1/messages", g.key, testkit.AnthropicBody(), map[string]string{
		"X-Request-Id": marker, "X-Comporta-User": user, "Baggage": "user=" + user, "Tracestate": "canary=" + marker,
		"Traceparent": "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01",
	}), http.StatusOK)
	expect("telemetry-invalid-user", g.send("telemetry-invalid-user", "/v1/messages", g.strictKey, testkit.SpacedID(), testkit.AnthropicBody()), http.StatusBadRequest)
	expect("telemetry-missing-user", g.send("telemetry-missing-user", "/v1/messages", g.strictKey, "", testkit.AnthropicBody()), http.StatusBadRequest)
	expect("telemetry-limited", g.send("telemetry-limited", "/v1/messages", g.limitedKey, user, testkit.AnthropicBody()), http.StatusOK)
	expect("telemetry-limited-again", g.send("telemetry-limited-again", "/v1/messages", g.limitedKey, user, testkit.AnthropicBody()), http.StatusTooManyRequests)
	expect("telemetry-bad-key", g.send("telemetry-bad-key", "/v1/messages", "cmp_"+marker, user, testkit.AnthropicBody()), http.StatusUnauthorized)

	// The scrape is served on the admin listener, behind the admin token.
	request, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1:"+itoa(g.adminPort)+"/metrics", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	exposition, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || !bytes.Contains(exposition, []byte(`comporta_blocks_total{app="limited",scope="user",type="rate"} 1`)) ||
		!bytes.Contains(exposition, []byte(`comporta_upstream_attempts_total{app="bot",model="fake-gpt",provider="openai",result="server_error"} 1`)) {
		t.Fatalf("scrape %d:\n%s", response.StatusCode, exposition)
	}
	run.scanner.Scan("metrics", "scrape", exposition)
	if status := g.sendWith("telemetry-proxy-scrape", "/metrics", g.key, "", map[string]string{}); status != http.StatusNotFound {
		t.Fatalf("the proxy listener answers the scrape with %d", status)
	}

	bodies := receiver.waitRoots(t, sent)
	for i, body := range bodies {
		run.scanner.Scan("traces", "otlp-"+itoa(i), body)
	}
	g.stop()

	db, err := sql.Open("sqlite", filepath.Join(g.dir, "data", "usage.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT DISTINCT user_hash FROM requests WHERE user_hash IS NOT NULL`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	hashes := 0
	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			t.Fatal(err)
		}
		hashes++
		if bytes.Contains(exposition, []byte(hash)) {
			t.Fatal("a user hash is in the metrics")
		}
		for _, body := range bodies {
			if bytes.Contains(body, []byte(hash)) {
				t.Fatal("a user hash is in the traces")
			}
		}
	}
	if hashes == 0 {
		t.Fatal("the control failed: no user hash was stored")
	}
}
