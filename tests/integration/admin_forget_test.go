package integration

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/fakeprovider"
	"github.com/JoaoVictorVM/comporta/internal/storage"
	"github.com/JoaoVictorVM/comporta/internal/storage/sqlite"
	"github.com/JoaoVictorVM/comporta/internal/testkit"
	"github.com/JoaoVictorVM/comporta/internal/useridentity"
)

type forgetReply struct {
	DeletedRows int64 `json:"deleted_rows"`
	Deleted     struct {
		Requests   int64 `json:"requests"`
		UsageDaily int64 `json:"usage_daily"`
		Events     int64 `json:"events"`
	} `json:"deleted"`
	UnlinkedRows          int64 `json:"unlinked_rows"`
	ClearedMemorySubjects int   `json:"cleared_memory_subjects"`
}

func (s *served) forget(id string) (int, forgetReply, string) {
	s.t.Helper()
	body, _ := json.Marshal(map[string]string{"user_id": id})
	status, text := s.admin(http.MethodPost, "/admin/v1/users/forget", string(body))
	var reply forgetReply
	if status == http.StatusOK {
		if err := json.Unmarshal([]byte(text), &reply); err != nil {
			s.t.Fatal(err)
		}
	}
	return status, reply, text
}

func (s *served) hash(app, id string) string {
	s.t.Helper()
	identity, err := useridentity.Open(context.Background(), useridentity.Options{Dir: s.dataDir})
	if err != nil {
		s.t.Fatal(err)
	}
	h, err := identity.LookupHash(app, id)
	if err != nil {
		s.t.Fatal(err)
	}
	return h.Hex()
}

func (s *served) anonymousHash(app string) string {
	s.t.Helper()
	identity, err := useridentity.Open(context.Background(), useridentity.Options{Dir: s.dataDir})
	if err != nil {
		s.t.Fatal(err)
	}
	return identity.AnonymousHash(app).Hex()
}

// stored counts the materialized requests and daily rows of one pseudonym.
func (s *served) stored(app, hash string, anonymous bool) (requests, daily int) {
	s.t.Helper()
	reader, err := sqlite.OpenReader(context.Background(), s.dataDir)
	if err != nil {
		s.t.Fatal(err)
	}
	defer reader.Close()
	page, err := reader.Records(context.Background(), storage.RecordFilter{AppName: app, UserHash: hash, Anonymous: anonymous, Limit: 1000})
	if err != nil {
		s.t.Fatal(err)
	}
	rows, err := reader.Daily(context.Background(), storage.Filter{AppName: app, UserHash: hash, Anonymous: anonymous})
	if err != nil {
		s.t.Fatal(err)
	}
	return len(page.Records), len(rows)
}

func (s *served) appSpend(app string) storage.Daily {
	s.t.Helper()
	reader, err := sqlite.OpenReader(context.Background(), s.dataDir)
	if err != nil {
		s.t.Fatal(err)
	}
	defer reader.Close()
	rows, err := reader.Daily(context.Background(), storage.Filter{AppName: app})
	if err != nil {
		s.t.Fatal(err)
	}
	var total storage.Daily
	for _, row := range rows {
		total.Requests += row.Requests
		total.Cost += row.Cost
	}
	return total
}

// waitStored waits for the usage worker to materialize a request.
func (s *served) waitStored(app, hash string, anonymous bool, want int) {
	s.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if n, _ := s.stored(app, hash, anonymous); n >= want {
			return
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("usage of %s never materialized", app)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestForgetRemovesUserRecordsAndKeepsAppSpend(t *testing.T) {
	s := startServed(t, fakeprovider.Options{})
	id, other := testkit.UserMarker(), "someone-else"
	for _, user := range []string{id, id, other} {
		if status, body := s.call("bot", user, gptBody); status != http.StatusOK {
			t.Fatalf("%d %s", status, body)
		}
	}
	hash := s.hash("bot", id)
	s.waitStored("bot", hash, false, 2)
	s.waitStored("bot", s.hash("bot", other), false, 1)
	before := s.appSpend("bot")
	status, reply, text := s.forget(id)
	if status != http.StatusOK || reply.Deleted.Requests != 2 || reply.Deleted.UsageDaily != 1 || reply.Deleted.Events != 2 || reply.DeletedRows < 5 {
		t.Fatalf("forget: %d %s", status, text)
	}
	if strings.Contains(text, id) || strings.Contains(text, hash) || strings.Contains(text, "bot") {
		t.Fatalf("the reply names the user or its apps: %s", text)
	}
	if requests, daily := s.stored("bot", hash, false); requests != 0 || daily != 0 {
		t.Fatalf("records left: %d requests, %d daily", requests, daily)
	}
	if n, _ := s.stored("bot", s.hash("bot", other), false); n != 1 {
		t.Fatal("another user was deleted")
	}
	if after := s.appSpend("bot"); after != before {
		t.Fatalf("app totals changed: %+v -> %+v", before, after)
	}
	status, reply, _ = s.forget(id)
	if status != http.StatusOK || reply.DeletedRows != 0 {
		t.Fatalf("a repeated forget: %d %+v", status, reply)
	}
	// Forgetting never bans: new traffic starts new history.
	if status, _ := s.call("bot", id, gptBody); status != http.StatusOK {
		t.Fatal("the user was blocked after the deletion")
	}
	s.waitStored("bot", hash, false, 1)
}

func TestLookupAndForgetUseSameAppScopedHash(t *testing.T) {
	s := startServed(t, fakeprovider.Options{})
	id := testkit.UserMarker()
	s.call("bot", id, gptBody)
	s.call("other", id, gptBody)
	botHash, otherHash := s.hash("bot", id), s.hash("other", id)
	if botHash == otherHash {
		t.Fatal("pseudonyms must be scoped by app")
	}
	s.waitStored("bot", botHash, false, 1)
	s.waitStored("other", otherHash, false, 1)
	if status, reply, text := s.forget(id); status != http.StatusOK || reply.Deleted.Requests != 2 {
		t.Fatalf("one forget must cover every app: %d %s", status, text)
	}
	for app, hash := range map[string]string{"bot": botHash, "other": otherHash} {
		if n, _ := s.stored(app, hash, false); n != 0 {
			t.Fatalf("%s kept the user", app)
		}
	}
	if status, _, text := s.forget(testkit.EmailID()); status != http.StatusBadRequest || strings.Contains(text, "@") {
		t.Fatalf("an email as user ID: %d %s", status, text)
	}
}

func TestForgetIncludesRemovedApps(t *testing.T) {
	s := startServed(t, fakeprovider.Options{})
	id := testkit.UserMarker()
	s.call("other", id, gptBody)
	hash := s.hash("other", id)
	s.waitStored("other", hash, false, 1)
	text, err := os.ReadFile(s.path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.path, []byte(removeApp(string(text), "other")), 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if status, _ := s.call("other", "probe-user", gptBody); status == http.StatusUnauthorized || status == http.StatusForbidden {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the app was not removed")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if status, reply, body := s.forget(id); status != http.StatusOK || reply.Deleted.Requests != 1 {
		t.Fatalf("a removed app was skipped: %d %s", status, body)
	}
	if n, _ := s.stored("other", hash, false); n != 0 {
		t.Fatal("the removed app kept the user")
	}
}

func TestForgetKeepsAnonymousBucket(t *testing.T) {
	s := startServed(t, fakeprovider.Options{})
	s.call("bot", "", gptBody)
	s.call("bot", "anonymous", gptBody)
	anon, named := s.anonymousHash("bot"), s.hash("bot", "anonymous")
	s.waitStored("bot", anon, true, 1)
	s.waitStored("bot", named, false, 1)
	if status, reply, body := s.forget("anonymous"); status != http.StatusOK || reply.Deleted.Requests != 1 {
		t.Fatalf("forget anonymous: %d %s", status, body)
	}
	if n, _ := s.stored("bot", anon, true); n != 1 {
		t.Fatal("the anonymous bucket was deleted by a user called anonymous")
	}
	if n, _ := s.stored("bot", named, false); n != 0 {
		t.Fatal("the user called anonymous survived")
	}
}

// heldStream starts a stream whose first event waits until released.
type heldStream struct {
	received chan struct{}
	release  chan struct{}
	once     sync.Once
}

func newHeldStream() *heldStream {
	return &heldStream{received: make(chan struct{}, 4), release: make(chan struct{})}
}

func (h *heldStream) options() fakeprovider.Options {
	return fakeprovider.Options{
		Scenario: fakeprovider.ScenarioSlowStream,
		OnRequest: func(fakeprovider.Request) {
			select {
			case h.received <- struct{}{}:
			default:
			}
		},
		Sleep: func(ctx context.Context, _ time.Duration) error {
			held := false
			h.once.Do(func() { held = true })
			if !held {
				return nil
			}
			select {
			case <-h.release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	}
}

func (s *served) stream(app, user string) <-chan int {
	done := make(chan int, 1)
	go func() {
		request, _ := http.NewRequest(http.MethodPost, s.proxyURL("/v1/chat/completions"), strings.NewReader(testkit.OpenAIStreamBody()))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+s.keys[app])
		request.Header.Set("X-Comporta-User", user)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			done <- 0
			return
		}
		defer func() { _ = response.Body.Close() }()
		_, _ = io.ReadAll(response.Body)
		done <- response.StatusCode
	}()
	return done
}

func TestForgetDrainsBeforeCommit(t *testing.T) {
	held := newHeldStream()
	s := startServed(t, held.options())
	id := testkit.UserMarker()
	finished := s.stream("bot", id)
	<-held.received
	type result struct {
		status int
		reply  forgetReply
		text   string
	}
	forgot := make(chan result, 1)
	go func() {
		status, reply, text := s.forget(id)
		forgot <- result{status, reply, text}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		status, body := s.call("bot", id, gptBody)
		if status == http.StatusServiceUnavailable && strings.Contains(body, "user_maintenance") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the barrier never held the user back: %d %s", status, body)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if status, body := s.call("bot", "someone-else", gptBody); status != http.StatusOK {
		t.Fatalf("other users must keep working during a deletion: %d %s", status, body)
	}
	close(held.release)
	if status := <-finished; status != http.StatusOK {
		t.Fatalf("the admitted stream was cut: %d", status)
	}
	r := <-forgot
	if r.status != http.StatusOK || r.reply.Deleted.Requests < 1 {
		t.Fatalf("forget after the drain: %d %s", r.status, r.text)
	}
	if n, _ := s.stored("bot", s.hash("bot", id), false); n != 0 {
		t.Fatal("the drained request was written back after the deletion")
	}
}

func TestForgetTimesOutWithoutDeletion(t *testing.T) {
	if testing.Short() {
		t.Skip("waits the full 10 second preparation deadline")
	}
	held := newHeldStream()
	s := startServed(t, held.options())
	id := testkit.UserMarker()
	s.call("other", id, gptBody)
	otherHash := s.hash("other", id)
	s.waitStored("other", otherHash, false, 1)
	for len(held.received) > 0 {
		<-held.received
	}
	finished := s.stream("bot", id)
	<-held.received
	began := time.Now()
	status, _, text := s.forget(id)
	if status != http.StatusConflict || !strings.Contains(text, "user_busy") {
		t.Fatalf("a request that outlives the deadline: %d %s", status, text)
	}
	if elapsed := time.Since(began); elapsed < 9*time.Second || elapsed > 20*time.Second {
		t.Fatalf("the preparation deadline was %v", elapsed)
	}
	if n, _ := s.stored("other", otherHash, false); n != 1 {
		t.Fatal("a deletion that gave up removed records")
	}
	if status, _ := s.call("other", id, gptBody); status != http.StatusOK {
		t.Fatal("the barrier outlived the failed deletion")
	}
	close(held.release)
	<-finished
}

func TestForgetCrashAfterCommitDoesNotRestoreUser(t *testing.T) {
	s := startServed(t, fakeprovider.Options{})
	id := testkit.UserMarker()
	s.call("bot", id, gptBody)
	hash := s.hash("bot", id)
	s.waitStored("bot", hash, false, 1)
	if status, _, text := s.forget(id); status != http.StatusOK {
		t.Fatalf("forget: %s", text)
	}
	s.restart()
	if requests, daily := s.stored("bot", hash, false); requests != 0 || daily != 0 {
		t.Fatal("recovery restored the user")
	}
}

func TestForgetPreservesAppBudgetAfterRecovery(t *testing.T) {
	s := startServed(t, fakeprovider.Options{})
	text, _ := os.ReadFile(s.path)
	budgeted := strings.Replace(string(text), "  bot:\n    allowed_models: [fake-claude, fake-gpt]\n",
		"  bot:\n    allowed_models: [fake-claude, fake-gpt]\n    budget: {usd_per_day: 100}\n    per_user:\n      budget: {usd_per_day: 50}\n", 1)
	if budgeted == string(text) {
		t.Fatal("could not add the budget")
	}
	if err := os.WriteFile(s.path, []byte(budgeted), 0o600); err != nil {
		t.Fatal(err)
	}
	s.restart()
	id := testkit.UserMarker()
	remaining := func(user string) string {
		request, _ := http.NewRequest(http.MethodPost, s.proxyURL("/v1/chat/completions"), strings.NewReader(gptBody))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+s.keys["bot"])
		request.Header.Set("X-Comporta-User", user)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = response.Body.Close() }()
		_, _ = io.ReadAll(response.Body)
		if response.StatusCode != http.StatusOK {
			t.Fatalf("status %d", response.StatusCode)
		}
		return response.Header.Get("X-Comporta-Budget-Limit") + "=" + response.Header.Get("X-Comporta-Budget-Remaining")
	}
	remaining(id)
	remaining(id)
	s.waitStored("bot", s.hash("bot", id), false, 2)
	before := s.appSpend("bot")
	if status, _, body := s.forget(id); status != http.StatusOK {
		t.Fatalf("forget: %s", body)
	}
	s.restart()
	if after := s.appSpend("bot"); after != before {
		t.Fatalf("app totals after recovery: %+v -> %+v", before, after)
	}
	// The app budget still counts the deleted user's spending: a new user sees
	// the same app balance as before, minus its own reservation.
	header := remaining("fresh-user")
	if header == "=" || !strings.HasPrefix(header, "apps.bot.") {
		t.Fatalf("no budget headers: %q", header)
	}
}
