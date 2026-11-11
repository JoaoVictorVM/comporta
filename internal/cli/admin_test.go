package cli

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

const cliToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// fakeAPI records the calls and answers with canned JSON.
type fakeAPI struct {
	mu       sync.Mutex
	server   *httptest.Server
	auth     []string
	paths    []string
	bodies   []map[string]any
	status   int
	response func(path string, body map[string]any) string
}

func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()
	f := &fakeAPI{status: http.StatusOK}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(data, &body)
		f.mu.Lock()
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		f.paths = append(f.paths, r.Method+" "+r.URL.RequestURI())
		f.bodies = append(f.bodies, body)
		status, respond := f.status, f.response
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if respond != nil {
			_, _ = io.WriteString(w, respond(r.URL.Path, body))
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}

func TestPauseCLIUsesBearer(t *testing.T) {
	api := newFakeAPI(t)
	api.response = func(string, map[string]any) string {
		return `{"app":"bot-discord","global_paused":false,"app_paused":true,"effective_paused":true,"changed":true,"updated_at":"2026-11-08T06:12:44Z"}`
	}
	t.Setenv(tokenEnvironment, cliToken)
	code, out, stderr := run(t, "pause", "bot-discord", "--admin-url", api.server.URL)
	if code != 0 || stderr != "" {
		t.Fatalf("%d %q %q", code, out, stderr)
	}
	if !strings.HasPrefix(out, "bot-discord paused at ") || !strings.HasSuffix(out, "; resume with: comporta resume bot-discord\n") {
		t.Fatalf("output %q", out)
	}
	if api.auth[0] != "Bearer "+cliToken || api.paths[0] != "POST /admin/v1/pause" || api.bodies[0]["app"] != "bot-discord" {
		t.Fatalf("call %v %v %v", api.auth, api.paths, api.bodies)
	}
	if strings.Contains(out, cliToken) {
		t.Fatal("the token was printed")
	}

	api.response = func(string, map[string]any) string {
		return `{"app":"bot-discord","global_paused":true,"app_paused":false,"effective_paused":true,"changed":true,"updated_at":"2026-11-08T06:12:44Z"}`
	}
	if code, out, _ := run(t, "resume", "bot-discord", "--admin-url", api.server.URL); code != 0 || !strings.Contains(out, "remains paused by the global pause") {
		t.Fatalf("resume under a global pause: %d %q", code, out)
	}
	api.response = func(string, map[string]any) string {
		return `{"all":true,"global_paused":true,"changed":true,"updated_at":"2026-11-08T06:12:44Z"}`
	}
	if code, out, _ := run(t, "pause", "--all", "--admin-url", api.server.URL); code != 0 || !strings.Contains(out, "all apps paused at ") || api.bodies[2]["all"] != true {
		t.Fatalf("pause --all: %d %q", code, out)
	}
	for _, args := range [][]string{{"pause"}, {"pause", "a", "b"}, {"pause", "bot", "--all"}, {"resume"}} {
		if code, _, _ := run(t, append(args, "--admin-url", api.server.URL)...); code != 1 {
			t.Fatalf("%v accepted", args)
		}
	}
}

func TestCLIFlagOverridesEnvironment(t *testing.T) {
	api := newFakeAPI(t)
	api.response = func(string, map[string]any) string {
		return `{"all":true,"global_paused":false,"changed":true,"updated_at":"2026-11-08T06:12:44Z"}`
	}
	t.Setenv(tokenEnvironment, strings.Repeat("e", 64))
	if code, _, stderr := run(t, "resume", "--all", "--token", cliToken, "--admin-url", api.server.URL); code != 0 {
		t.Fatal(stderr)
	}
	if api.auth[0] != "Bearer "+cliToken {
		t.Fatalf("the flag must win over the environment: %q", api.auth[0])
	}
	t.Setenv(tokenEnvironment, "")
	if code, _, stderr := run(t, "resume", "--all", "--admin-url", api.server.URL); code != 1 || !strings.Contains(stderr, tokenEnvironment) {
		t.Fatalf("no token: %d %q", code, stderr)
	}
	for _, bad := range []string{"http://user:pass@127.0.0.1:1", "http://127.0.0.1:1/?token=x", "http://127.0.0.1:1/#f", "http://example.com:8081", "ftp://127.0.0.1:1", "http://127.0.0.1:1/admin"} {
		if code, _, stderr := run(t, "resume", "--all", "--token", cliToken, "--admin-url", bad); code != 1 || strings.Contains(stderr, "pass") || strings.Contains(stderr, cliToken) {
			t.Fatalf("%s: %d %q", bad, code, stderr)
		}
	}
}

func closedPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	return port
}

func TestCLIUnreachableMessage(t *testing.T) {
	t.Setenv(tokenEnvironment, cliToken)
	port := closedPort(t)
	text := strings.Replace(validConfig, "apps:", "listeners:\n  admin: {port: "+strconv.Itoa(port)+"}\napps:", 1)
	path := writeConfig(t, text)
	code, _, stderr := run(t, "pause", "bot", "--config", path)
	want := "error: cannot reach Comporta admin API at 127.0.0.1:" + strconv.Itoa(port) + "; is the gateway running?\n"
	if code != 1 || stderr != want {
		t.Fatalf("%d %q", code, stderr)
	}
	wildcard := strings.Replace(validConfig, "apps:", "listeners:\n  admin: {bind: 0.0.0.0, port: "+strconv.Itoa(port)+"}\napps:", 1)
	if _, _, stderr := run(t, "pause", "bot", "--config", writeConfig(t, wildcard)); !strings.Contains(stderr, "127.0.0.1:"+strconv.Itoa(port)) {
		t.Fatalf("a wildcard bind must be reached on loopback: %q", stderr)
	}
}

func TestCLIUnknownOutcomeAfterDisconnect(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			buffer := make([]byte, 4096)
			_, _ = conn.Read(buffer)
			_ = conn.Close()
		}
	}()
	t.Setenv(tokenEnvironment, cliToken)
	code, _, stderr := run(t, "users", "forget", "user_42", "--admin-url", "http://"+listener.Addr().String())
	if code != 1 || !strings.Contains(stderr, "outcome is unknown") || strings.Contains(stderr, "no records were removed") {
		t.Fatalf("%d %q", code, stderr)
	}
}

func TestSensitiveArgumentsNeverEcho(t *testing.T) {
	api := newFakeAPI(t)
	t.Setenv(tokenEnvironment, cliToken)
	secretID := "cmpsecret-user-77"
	secretToken := strings.Repeat("f", 64)
	for _, args := range [][]string{
		{"users", "forget", secretID, "extra-" + secretID},
		{"users", "forget", "--tokn=" + secretToken, secretID},
		{"users", "forget", secretID, "--token"},
		{"pause", "--tokn", secretToken, "bot"},
		{"usage", "--user", secretID, "--anonymous", "--admin-url", api.server.URL},
		{"usage", "--frm", secretID},
	} {
		code, out, stderr := run(t, args...)
		if code != 1 || strings.Contains(out+stderr, secretID) || strings.Contains(out+stderr, secretToken) {
			t.Fatalf("%v: %d %q %q", args, code, out, stderr)
		}
	}
	api.status = http.StatusBadRequest
	api.response = func(string, map[string]any) string {
		return `{"error":{"code":"invalid_request","message":"user_id is not a valid user ID"}}`
	}
	code, out, stderr := run(t, "users", "forget", secretID+"@mail.example", "--admin-url", api.server.URL)
	if code != 1 || strings.Contains(out+stderr, secretID) || !strings.Contains(stderr, "user_id is not a valid user ID") {
		t.Fatalf("%d %q %q", code, out, stderr)
	}
}

func TestForgetCLIReportsCounts(t *testing.T) {
	api := newFakeAPI(t)
	t.Setenv(tokenEnvironment, cliToken)
	api.response = func(string, map[string]any) string {
		return `{"deleted_rows":0,"deleted":{"requests":0,"usage_daily":0,"events":0,"journal":0,"budget_state":0,"reservation_items":0},"unlinked_rows":0,"cleared_memory_subjects":0}`
	}
	if code, out, _ := run(t, "users", "forget", "user_42", "--admin-url", api.server.URL); code != 0 || out != "no records found for this user ID\n" {
		t.Fatalf("%d %q", code, out)
	}
	api.response = func(string, map[string]any) string {
		return `{"deleted_rows":16,"deleted":{"requests":10,"usage_daily":3,"events":1,"journal":0,"budget_state":2,"reservation_items":0},"unlinked_rows":4,"cleared_memory_subjects":2}`
	}
	code, out, _ := run(t, "users", "forget", "user_42", "--admin-url", api.server.URL)
	if code != 0 || !strings.Contains(out, "deleted 16 rows") || strings.Contains(out, "user_42") {
		t.Fatalf("%d %q", code, out)
	}
	if api.bodies[1]["user_id"] != "user_42" || api.paths[1] != "POST /admin/v1/users/forget" {
		t.Fatal("the ID must travel only in the body")
	}
	api.status = http.StatusServiceUnavailable
	api.response = func(string, map[string]any) string {
		return `{"error":{"code":"deletion_failed","message":"deletion failed; no records were removed"}}`
	}
	if code, _, stderr := run(t, "users", "forget", "user_42", "--admin-url", api.server.URL); code != 1 || stderr != "error: deletion failed; no records were removed\n" {
		t.Fatalf("%d %q", code, stderr)
	}
}
