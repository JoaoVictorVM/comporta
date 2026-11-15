package integration

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/events"
	"github.com/JoaoVictorVM/comporta/internal/fakeprovider"
	"github.com/JoaoVictorVM/comporta/internal/storage/sqlite"
	"github.com/JoaoVictorVM/comporta/internal/testkit"
)

func sensitiveChat(text string) string {
	encoded, _ := json.Marshal(testkit.Prompt(text))
	return `{"model":"fake-gpt","messages":[{"role":"user","content":` + string(encoded) + `}]}`
}

// sensitiveEventsOf reads the sensitive data events of one pseudonym, through a
// reader of its own as an operator would.
func (s *served) sensitiveEventsOf(hash string) []events.Event {
	s.t.Helper()
	reader, err := sqlite.OpenReader(context.Background(), s.dataDir)
	if err != nil {
		s.t.Fatal(err)
	}
	defer reader.Close()
	page, err := reader.ReadEvents(context.Background(), events.Filter{Types: []events.Type{events.SensitiveDataDetected}, Limit: 1000})
	if err != nil {
		s.t.Fatal(err)
	}
	var out []events.Event
	for _, e := range page.Events {
		if e.UserHash == hash {
			out = append(out, e)
		}
	}
	return out
}

// TestForgetRemovesOrphanSensitiveEvent checks that users forget removes the
// events of a request that never reached the provider, together with its own.
func TestForgetRemovesOrphanSensitiveEvent(t *testing.T) {
	s := startServedWith(t, fakeprovider.Options{}, withDetection("email: block", "cpf: redact"))
	id, other := testkit.UserMarker(), "someone-else"
	if status, body := s.call("bot", id, sensitiveChat("para cliente@example.test")); status != http.StatusBadRequest || !strings.Contains(body, "sensitive_data_blocked") {
		t.Fatalf("blocked request: %d %s", status, body)
	}
	if status, body := s.call("bot", id, sensitiveChat("cpf "+testkit.DocumentID())); status != http.StatusOK {
		t.Fatalf("redacted request: %d %s", status, body)
	}
	if status, body := s.call("bot", other, sensitiveChat("cpf "+testkit.DocumentID())); status != http.StatusOK {
		t.Fatalf("other user: %d %s", status, body)
	}
	hash := s.hash("bot", id)
	s.waitStored("bot", hash, false, 2)
	if got := s.sensitiveEventsOf(hash); len(got) != 2 {
		t.Fatalf("events before forget: %d", len(got))
	}
	status, reply, text := s.forget(id)
	if status != http.StatusOK || reply.Deleted.Events < 2 {
		t.Fatalf("forget: %d %s", status, text)
	}
	if got := s.sensitiveEventsOf(hash); len(got) != 0 {
		t.Fatalf("events left after forget: %+v", got)
	}
	if got := s.sensitiveEventsOf(s.hash("bot", other)); len(got) != 1 {
		t.Fatalf("another user's event was removed: %d", len(got))
	}
}

// TestForgetWaitsForScan holds a stream whose event was published under the gate
// ticket: the deletion waits for it, and nothing reappears afterwards.
func TestForgetWaitsForScan(t *testing.T) {
	held := newHeldStream()
	s := startServedWith(t, held.options(), withDetection("cpf: redact"))
	id := testkit.UserMarker()
	finished := make(chan int, 1)
	go func() {
		body := strings.Replace(testkit.OpenAIStreamBody(), `"messages":[`, `"messages":[{"role":"user","content":"cpf `+testkit.DocumentID()+`"},`, 1)
		request, _ := http.NewRequest(http.MethodPost, s.proxyURL("/v1/chat/completions"), strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+s.keys["bot"])
		request.Header.Set("X-Comporta-User", id)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			finished <- 0
			return
		}
		defer func() { _ = response.Body.Close() }()
		_, _ = io.ReadAll(response.Body)
		finished <- response.StatusCode
	}()
	<-held.received
	hash := s.hash("bot", id)
	if got := s.sensitiveEventsOf(hash); len(got) != 1 {
		t.Fatalf("the event must be durable before the provider is called: %d", len(got))
	}
	forgot := make(chan int, 1)
	go func() {
		status, _, _ := s.forget(id)
		forgot <- status
	}()
	select {
	case status := <-forgot:
		t.Fatalf("forget did not wait for the request in flight: %d", status)
	case <-time.After(300 * time.Millisecond):
	}
	close(held.release)
	if status := <-finished; status != http.StatusOK {
		t.Fatalf("the admitted stream was cut: %d", status)
	}
	if status := <-forgot; status != http.StatusOK {
		t.Fatalf("forget after the drain: %d", status)
	}
	if got := s.sensitiveEventsOf(hash); len(got) != 0 {
		t.Fatal("the event survived the deletion")
	}
	if n, _ := s.stored("bot", hash, false); n != 0 {
		t.Fatal("the drained request was written back after the deletion")
	}
}

// TestHMACResetRemovesSensitiveSubject checks the identity reset unlinks the
// subject of sensitive data events like every other per-user row.
func TestHMACResetRemovesSensitiveSubject(t *testing.T) {
	s := startServedWith(t, fakeprovider.Options{}, withDetection("email: block"))
	id := testkit.UserMarker()
	if status, body := s.call("bot", id, sensitiveChat("para cliente@example.test")); status != http.StatusBadRequest {
		t.Fatalf("blocked request: %d %s", status, body)
	}
	hash := s.hash("bot", id)
	if got := s.sensitiveEventsOf(hash); len(got) != 1 {
		t.Fatalf("events before reset: %d", len(got))
	}
	s.shutdown()
	store, err := sqlite.Open(context.Background(), s.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Reset(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if got := s.sensitiveEventsOf(hash); len(got) != 0 {
		t.Fatalf("the reset kept the subject: %+v", got)
	}
}
