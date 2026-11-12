package integration

import (
	"net/http"
	"strings"
	"testing"

	"github.com/JoaoVictorVM/comporta/internal/fakeprovider"
	"github.com/JoaoVictorVM/comporta/internal/testkit"
)

// loopOut sends similar requests until the eleventh is cut.
func (s *served) loopOut(app, id string) {
	s.t.Helper()
	for i := range 10 {
		if status, body := s.call(app, id, gptBody); status != http.StatusOK {
			s.t.Fatalf("similar request %d: %d %s", i+1, status, body)
		}
	}
	if status, body := s.call(app, id, gptBody); status != http.StatusTooManyRequests || !strings.Contains(body, "loop_detected") {
		s.t.Fatalf("eleventh similar request: %d %s", status, body)
	}
}

func TestForgetClearsLoopState(t *testing.T) {
	s := startServed(t, fakeprovider.Options{})
	id := testkit.UserMarker()
	s.loopOut("bot", id)
	if status, _, text := s.forget(id); status != http.StatusOK {
		t.Fatalf("forget: %d %s", status, text)
	}
	if status, body := s.call("bot", id, gptBody); status != http.StatusOK {
		t.Fatalf("the cooldown outlived the deletion: %d %s", status, body)
	}
	// The anonymous subject and other users were never touched.
	if status, body := s.call("bot", "someone-else", gptBody); status != http.StatusOK {
		t.Fatalf("another user: %d %s", status, body)
	}
}

func TestPausePrecedesLoopGuard(t *testing.T) {
	s := startServed(t, fakeprovider.Options{})
	id := testkit.UserMarker()
	if status, body := s.admin(http.MethodPost, "/admin/v1/pause", `{"app":"bot"}`); status != http.StatusOK {
		t.Fatalf("pause: %d %s", status, body)
	}
	for range 15 {
		if status, body := s.call("bot", id, gptBody); status != http.StatusServiceUnavailable || !strings.Contains(body, "app_paused") {
			t.Fatalf("paused app: %d %s", status, body)
		}
	}
	if status, body := s.admin(http.MethodPost, "/admin/v1/resume", `{"app":"bot"}`); status != http.StatusOK {
		t.Fatalf("resume: %d %s", status, body)
	}
	// Refusals by the pause never reached the guard: the count starts now.
	s.loopOut("bot", id)
}

func TestFailedForgetKeepsLoopState(t *testing.T) {
	if testing.Short() {
		t.Skip("waits the full 10 second preparation deadline")
	}
	held := newHeldStream()
	s := startServed(t, held.options())
	id := testkit.UserMarker()
	for i := range 10 {
		if status, body := s.call("bot", id, gptBody); status != http.StatusOK {
			t.Fatalf("similar request %d: %d %s", i+1, status, body)
		}
	}
	for len(held.received) > 0 {
		<-held.received
	}
	// A stream the provider holds open keeps the deletion from draining, so it
	// gives up without deleting anything.
	finished := s.stream("bot", id)
	<-held.received
	if status, _, text := s.forget(id); status != http.StatusConflict {
		t.Fatalf("forget with a request in flight: %d %s", status, text)
	}
	close(held.release)
	if status := <-finished; status != http.StatusOK {
		t.Fatalf("the admitted stream: %d", status)
	}
	if status, body := s.call("bot", id, gptBody); status != http.StatusTooManyRequests || !strings.Contains(body, "loop_detected") {
		t.Fatalf("a failed deletion cleared the loop state: %d %s", status, body)
	}
}
