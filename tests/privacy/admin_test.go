package privacy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/storage"
	"github.com/JoaoVictorVM/comporta/internal/storage/sqlite"
	"github.com/JoaoVictorVM/comporta/internal/testkit"
)

// adminCall sends one admin API call and scans the answer like any error.
func (g *liveGateway) adminCall(name, token, method, path string, body any) (int, []byte) {
	g.t.Helper()
	var reader io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		reader = strings.NewReader(string(data))
	}
	request, err := http.NewRequest(method, "http://127.0.0.1:"+itoa(g.adminPort)+path, reader)
	if err != nil {
		g.t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		g.t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	data, _ := io.ReadAll(response.Body)
	run.scanner.Scan("errors", name, data)
	return response.StatusCode, data
}

func itoa(n int) string {
	data, _ := json.Marshal(n)
	return string(data)
}

func TestAdminOperationsNeverPersistRawIdentityOrToken(t *testing.T) {
	run.markCase("process/admin-operations")
	g := startLive(t, "success", "")
	defer g.stop()
	serveStdout, serveStderr := g.logs[2], g.logs[3]
	var token string
	for deadline := time.Now().Add(10 * time.Second); token == "" && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		data, _ := os.ReadFile(serveStdout)
		if match := regexp.MustCompile(`store it now\): ([0-9a-f]{64})`).FindSubmatch(data); match != nil {
			token = string(match[1])
		}
	}
	if token == "" {
		t.Fatal("the admin token was not shown on the first start")
	}
	run.markConstructor("AnthropicBody")
	user := testkit.UserMarker()
	if status := g.send("admin-traffic", "/v1/messages", g.key, user, testkit.AnthropicBody()); status != http.StatusOK {
		t.Fatalf("status %d", status)
	}
	reader, err := sqlite.OpenReader(context.Background(), filepath.Join(g.dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		page, err := reader.Records(context.Background(), storage.RecordFilter{AppName: "bot", Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Records) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("positive control: the request was not recorded")
		}
	}
	reader.Close()

	if status, _ := g.adminCall("admin-pause", token, http.MethodPost, "/admin/v1/pause", map[string]string{"app": "bot"}); status != http.StatusOK {
		t.Fatalf("pause %d", status)
	}
	if status := g.send("admin-paused", "/v1/messages", g.key, user, testkit.AnthropicBody()); status != http.StatusServiceUnavailable {
		t.Fatalf("positive control: the pause did not refuse, status %d", status)
	}
	g.adminCall("admin-resume", token, http.MethodPost, "/admin/v1/resume", map[string]string{"app": "bot"})
	if status, data := g.adminCall("admin-usage", token, http.MethodPost, "/admin/v1/usage/query", map[string]string{"user_id": user}); status != http.StatusOK || !strings.Contains(string(data), `"requests":"1"`) {
		t.Fatalf("positive control: usage by user %d %s", status, data)
	}
	if status, _ := g.adminCall("admin-forget-email", token, http.MethodPost, "/admin/v1/users/forget", map[string]string{"user_id": testkit.EmailID()}); status != http.StatusBadRequest {
		t.Fatalf("an email ID: %d", status)
	}
	// A settlement whose write missed its deadline keeps the reservation open
	// until the limit worker retries it, every 5 seconds; until then the
	// deletion is refused with 409 and the operator tries again.
	var status int
	var data []byte
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(500 * time.Millisecond) {
		status, data = g.adminCall("admin-forget", token, http.MethodPost, "/admin/v1/users/forget", map[string]string{"user_id": user})
		if status != http.StatusConflict || time.Now().After(deadline) {
			break
		}
	}
	if status != http.StatusOK || !strings.Contains(string(data), `"requests":1`) {
		logs, _ := os.ReadFile(serveStderr)
		t.Fatalf("positive control: forget %d %s\ngateway log:\n%s", status, data, logs)
	}
	if status, _ := g.adminCall("admin-wrong-token", user, http.MethodGet, "/admin/v1/pause", nil); status != http.StatusUnauthorized {
		t.Fatalf("a marker as token: %d", status)
	}
	g.stop()

	// The token may appear once, in the authorized display; nowhere else.
	tokens := testkit.NewScanner(token)
	for _, name := range []string{"usage.sqlite", "usage.sqlite-wal", "usage.sqlite-shm"} {
		tokens.ScanFile("database", filepath.Join(g.dir, "data", name))
	}
	tokens.ScanDir("files", filepath.Join(g.dir, "data"))
	tokens.ScanFile("logs", serveStderr)
	if err := tokens.Err("database", "files", "logs"); err != nil {
		t.Fatalf("the admin token leaked: %v", err)
	}
	shown, _ := os.ReadFile(serveStdout)
	if strings.Count(string(shown), token) != 1 {
		t.Fatal("the token must be shown exactly once")
	}
}
