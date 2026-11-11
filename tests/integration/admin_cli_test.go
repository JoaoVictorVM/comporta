package integration

import (
	"bytes"
	"context"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/JoaoVictorVM/comporta/internal/cli"
	"github.com/JoaoVictorVM/comporta/internal/fakeprovider"
	"github.com/JoaoVictorVM/comporta/internal/testkit"
)

func (s *served) cli(args ...string) (int, string, string) {
	s.t.Helper()
	var stdout, stderr bytes.Buffer
	code := cli.Execute(context.Background(), append(args, "--config", s.path), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

var pausedLine = regexp.MustCompile(`^bot paused at \d{2}:\d{2}:\d{2}; resume with: comporta resume bot\n$`)

func TestCLIOperatesRunningGateway(t *testing.T) {
	s := startServed(t, fakeprovider.Options{})
	t.Setenv("COMPORTA_ADMIN_TOKEN", s.token)

	code, out, stderr := s.cli("pause", "bot")
	if code != 0 || !pausedLine.MatchString(out) {
		t.Fatalf("pause: %d %q %q", code, out, stderr)
	}
	status, body := s.call("bot", "", gptBody)
	expectPaused(t, status, body, "bot")
	if code, out, _ := s.cli("resume", "bot"); code != 0 || !strings.HasPrefix(out, "bot resumed at ") {
		t.Fatalf("resume: %d %q", code, out)
	}

	id := testkit.UserMarker()
	s.call("bot", id, gptBody)
	s.call("bot", id, gptBody)
	s.waitStored("bot", s.hash("bot", id), false, 2)
	code, out, stderr = s.cli("usage", "--app", "bot", "--user", id)
	if code != 0 || !strings.Contains(out, "total - 2 ") || strings.Contains(out, id) {
		t.Fatalf("usage: %d %q %q", code, out, stderr)
	}
	code, out, _ = s.cli("usage", "--json")
	if code != 0 || !strings.Contains(out, `"next_cursor": null`) || !strings.Contains(out, `"app": "bot"`) {
		t.Fatalf("usage --json: %d %q", code, out)
	}
	code, out, stderr = s.cli("users", "forget", id)
	if code != 0 || !strings.HasPrefix(out, "deleted ") || strings.Contains(out+stderr, id) {
		t.Fatalf("forget: %d %q %q", code, out, stderr)
	}
	if code, out, _ := s.cli("users", "forget", id); code != 0 || out != "no records found for this user ID\n" {
		t.Fatalf("forget again: %d %q", code, out)
	}
	if code, _, stderr := s.cli("pause", "bot", "--token", strings.Repeat("0", 64)); code != 1 || stderr != "error: invalid admin token\n" {
		t.Fatalf("a wrong token: %d %q", code, stderr)
	}
	if code, _, stderr := s.cli("admin", "reset-token"); code != 1 || !strings.Contains(stderr, "stop the gateway") {
		t.Fatalf("reset with the gateway running: %d %q", code, stderr)
	}

	s.shutdown()
	if code, _, stderr := s.cli("pause", "bot"); code != 1 || !strings.Contains(stderr, "cannot reach Comporta admin API at 127.0.0.1:") || !strings.Contains(stderr, "is the gateway running?") {
		t.Fatalf("gateway stopped: %d %q", code, stderr)
	}
	code, out, stderr = s.cli("admin", "reset-token")
	match := regexp.MustCompile(`store it now\): ([0-9a-f]{64})\n$`).FindStringSubmatch(out)
	if code != 0 || match == nil || match[1] == s.token {
		t.Fatalf("reset: %d %q %q", code, out, stderr)
	}
	old := s.token
	s.start()
	if strings.Contains(s.stdout.String(), "store it now") {
		t.Fatal("the restart after a reset showed a token")
	}
	s.token = old
	if status, _ := s.admin(http.MethodGet, "/admin/v1/pause", ""); status != http.StatusUnauthorized {
		t.Fatal("the old token still works after a reset")
	}
	s.token = match[1]
	if status, _ := s.admin(http.MethodGet, "/admin/v1/pause", ""); status != http.StatusOK {
		t.Fatal("the new token does not work")
	}
}
