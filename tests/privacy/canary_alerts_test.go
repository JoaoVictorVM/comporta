package privacy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/alerts"
	"github.com/JoaoVictorVM/comporta/internal/testkit"
)

// alertReceiver stands for every external destination: it records the bodies
// and headers a channel received and answers like a healthy receiver, with an
// echo of a marker that must never travel back into the gateway's outputs.
type alertReceiver struct {
	mu     sync.Mutex
	bodies map[string][][]byte
	heads  []http.Header
}

func (a *alertReceiver) handler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	a.mu.Lock()
	a.bodies[r.URL.Path] = append(a.bodies[r.URL.Path], body)
	a.heads = append(a.heads, r.Header.Clone())
	a.mu.Unlock()
	if r.URL.Path == "/slack" {
		_, _ = io.WriteString(w, "ok")
		return
	}
	// A receiver may answer anything; its body never reaches a log or a row.
	_, _ = io.WriteString(w, "accepted "+testkit.UserMarker())
}

func (a *alertReceiver) received(kind string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, body := range a.bodies["/hook"] {
		if strings.Contains(string(body), `"type":"`+kind+`"`) {
			n++
		}
	}
	return n
}

// TestCanaryAbsentFromEveryAlertSurface drives a loop cut and a rejected
// reload with marked prompts, raw user IDs, a marked rejected configuration
// and a receiver that echoes a marker, then scans what the receivers got, the
// database, the logs and the files. Alerts carry names, numbers, times and
// pseudonyms only.
func TestCanaryAbsentFromEveryAlertSurface(t *testing.T) {
	run.markCase("process/alerts")
	receiver := &alertReceiver{bodies: map[string][][]byte{}}
	server := httptest.NewServer(http.HandlerFunc(receiver.handler))
	defer server.Close()
	const secret = "canary-alert-signing-secret"
	t.Setenv("COMPORTA_CANARY_HOOK_URL", server.URL+"/hook")
	t.Setenv("COMPORTA_CANARY_SLACK_URL", server.URL+"/slack")
	t.Setenv("COMPORTA_CANARY_HOOK_SECRET", secret)
	block := `alerts:
  thresholds: [50, 80, 100]
  channels:
    hook:
      type: webhook
      url: ${COMPORTA_CANARY_HOOK_URL}
      signing_secret: ${COMPORTA_CANARY_HOOK_SECRET}
    team:
      type: slack
      url: ${COMPORTA_CANARY_SLACK_URL}
`
	var config string
	g := startLiveFake(t, []string{"--scenario", "agent-loop"}, "", func(text string) string {
		config = text + block
		return config
	})
	defer g.stop()
	run.markConstructor("AnthropicBody")
	user := testkit.UserMarker()
	for i := range 13 {
		g.send(fmt.Sprintf("alerts-loop-%d", i), "/v1/messages", g.key, user, testkit.AnthropicBody())
	}
	// The rejected file carries a marker in a comment and an invalid value. The
	// alert may say that a reload was rejected, never what the file held.
	rejected := config + "# " + testkit.UserMarker() + "\ntelemetry:\n  otel_enabled: maybe\n"
	if err := os.WriteFile(filepath.Join(g.dir, "comporta.yaml"), []byte(rejected), 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if receiver.received(string(alerts.TriggerLoopCut)) > 0 && receiver.received(string(alerts.TriggerReloadFailed)) > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, kind := range []alerts.Trigger{alerts.TriggerLoopCut, alerts.TriggerReloadFailed} {
		if receiver.received(string(kind)) == 0 {
			t.Fatalf("positive control: no %s alert reached the receiver within 60s", kind)
		}
	}
	g.stop()
	receiver.mu.Lock()
	defer receiver.mu.Unlock()
	for path, bodies := range receiver.bodies {
		for i, body := range bodies {
			run.scanner.Scan("alerts", fmt.Sprintf("%s-%d", path, i), body)
			if strings.Contains(string(body), secret) || strings.Contains(string(body), server.URL) {
				t.Fatalf("alert body carries a secret or a URL: %s", path)
			}
		}
	}
	for i, h := range receiver.heads {
		var b strings.Builder
		for k, v := range h {
			b.WriteString(k + ": " + strings.Join(v, ",") + "\n")
		}
		run.scanner.Scan("alerts", fmt.Sprintf("headers-%d", i), []byte(b.String()))
	}
	if len(receiver.bodies["/slack"]) == 0 {
		t.Fatal("positive control: the formatted channel received nothing")
	}
}
