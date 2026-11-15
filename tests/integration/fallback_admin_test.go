package integration

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/events"
	"github.com/JoaoVictorVM/comporta/internal/fakeprovider"
	"github.com/JoaoVictorVM/comporta/internal/storage"
	"github.com/JoaoVictorVM/comporta/internal/testkit"
)

// fellBack makes one request through an overloaded first model and waits for
// both attempts to be materialized.
func (g *gateway) fellBack(t *testing.T, key, user string) string {
	t.Helper()
	response, body := g.callAs("/v1/messages", key, user, testkit.AnthropicBody())
	if response.StatusCode != 200 || response.Header.Get(fallbackHeaderName) == "" {
		t.Fatalf("%d %s", response.StatusCode, body)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, r := range g.records(t) {
			if r.AttemptIndex == 2 {
				return r.RequestID
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("attempts not materialized")
	return ""
}

func TestFallbackRecordsAndEventsReadable(t *testing.T) {
	gw := startFallbackGateway(t, "    fallback: [fake-haiku]\n", map[string]fakeprovider.Scenario{"fake-claude": fakeprovider.ScenarioOverload}, gatewayOptions{})
	key := gw.createKey("bot").Key.Reveal()
	user := testkit.UserMarker()
	request := gw.fellBack(t, key, user)
	subject := gw.userSubject(user)
	// The records of the user show both attempts, their roles and the cause.
	page, err := gw.usage.Records(context.Background(), storage.RecordFilter{AppName: "bot", UserHash: subject.Hash, Limit: 10})
	if err != nil || len(page.Records) != 2 {
		t.Fatalf("records %+v %v", page.Records, err)
	}
	attempts := byAttempt(page.Records, request)
	if attempts[1].RequestCount != 0 || attempts[2].RequestCount != 1 || attempts[2].FallbackCause != "529" || attempts[2].Outcome != "fallback" {
		t.Fatalf("attempts %+v", attempts)
	}
	// The aggregates count one request and two attempts, for the app and the user.
	for _, filter := range []storage.Filter{{AppName: "bot"}, {AppName: "bot", UserHash: subject.Hash}} {
		rows, err := gw.usage.Daily(context.Background(), filter)
		if err != nil || len(rows) != 1 || rows[0].Requests != 1 || rows[0].Attempts != 2 {
			t.Fatalf("daily %+v %v", rows, err)
		}
	}
	// Events: one transition, one completion per attempt, all readable by type.
	fallback := gw.policyEvents(t, events.Fallback)
	completed := gw.policyEvents(t, events.RequestCompleted)
	if len(fallback) != 1 || len(completed) != 2 || fallback[0].UserHash != "" {
		t.Fatalf("events %+v %+v", fallback, completed)
	}
	data, _ := json.Marshal(fallback[0])
	if strings.Contains(string(data), user) || strings.Contains(string(data), subject.Hash) {
		t.Fatalf("subject in the fallback event: %s", data)
	}
}

func TestFallbackForgetBarrier(t *testing.T) {
	gw := startFallbackGateway(t, "    fallback: [fake-haiku]\n", map[string]fakeprovider.Scenario{"fake-claude": fakeprovider.ScenarioOverload}, gatewayOptions{})
	key := gw.createKey("bot").Key.Reveal()
	user := testkit.UserMarker()
	gw.fellBack(t, key, user)
	subject := gw.userSubject(user)
	before, err := gw.usage.Daily(context.Background(), storage.Filter{AppName: "bot"})
	if err != nil || len(before) != 1 {
		t.Fatalf("daily %+v %v", before, err)
	}
	if _, err = gw.usage.ForgetSubjects(context.Background(), []storage.SubjectRef{{App: "bot", Hash: subject.Hash}}); err != nil {
		t.Fatal(err)
	}
	// A recovery afterwards brings nothing of the user back.
	if err = gw.usage.Recover(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	page, err := gw.usage.Records(context.Background(), storage.RecordFilter{AppName: "bot", Limit: 10})
	if err != nil || len(page.Records) != 0 {
		t.Fatalf("records after forget %+v %v", page.Records, err)
	}
	if n := len(gw.policyEvents(t, events.Fallback)); n != 0 {
		t.Fatalf("fallback events of a forgotten request: %d", n)
	}
	rows, err := gw.usage.Daily(context.Background(), storage.Filter{AppName: "bot", UserHash: subject.Hash})
	if err != nil || len(rows) != 0 {
		t.Fatalf("user aggregates %+v %v", rows, err)
	}
	// The app keeps what it spent.
	after, err := gw.usage.Daily(context.Background(), storage.Filter{AppName: "bot"})
	if err != nil || len(after) != 1 || after[0].Cost != before[0].Cost || after[0].Requests != 1 || after[0].Attempts != 2 {
		t.Fatalf("app aggregates %+v %v", after, err)
	}
}

func TestFallbackRetentionPreservesTotals(t *testing.T) {
	gw := startFallbackGateway(t, "    fallback: [fake-haiku]\n", map[string]fakeprovider.Scenario{"fake-claude": fakeprovider.ScenarioOverload}, gatewayOptions{})
	key := gw.createKey("bot").Key.Reveal()
	gw.fellBack(t, key, testkit.UserMarker())
	if err := gw.usage.Retain(context.Background(), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	page, err := gw.usage.Records(context.Background(), storage.RecordFilter{AppName: "bot", Limit: 10})
	if err != nil || len(page.Records) != 0 {
		t.Fatalf("records after retention %+v %v", page.Records, err)
	}
	rows, err := gw.usage.Daily(context.Background(), storage.Filter{AppName: "bot"})
	if err != nil || len(rows) != 1 || rows[0].Requests != 1 || rows[0].Attempts != 2 {
		t.Fatalf("totals after retention %+v %v", rows, err)
	}
	// A request that falls back after retention still materializes once.
	gw.fellBack(t, key, testkit.UserMarker()+"b")
	rows, err = gw.usage.Daily(context.Background(), storage.Filter{AppName: "bot"})
	if err != nil || rows[0].Requests != 2 || rows[0].Attempts != 4 {
		t.Fatalf("totals after a new request %+v %v", rows, err)
	}
}

// TestFallbackEventsReachSubscribers is the contract a burst alert consumes: one
// event per admitted transition, after commit, with app, models and cause.
func TestFallbackEventsReachSubscribers(t *testing.T) {
	gw := startFallbackGateway(t, "    fallback: [fake-haiku]\n", map[string]fakeprovider.Scenario{"fake-claude": fakeprovider.ScenarioOverload}, gatewayOptions{})
	key := gw.createKey("bot").Key.Reveal()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	subscription := gw.usage.Bus.Subscribe(ctx, events.Fallback)
	const burst = 5
	for i := range burst {
		gw.fellBack(t, key, testkit.UserMarker()+string(rune('a'+i)))
	}
	seen := map[string]bool{}
	timeout := time.After(5 * time.Second)
	for len(seen) < burst {
		select {
		case e := <-subscription.Events:
			if e.ValidateFallback() != nil || e.App != "bot" || e.Payload.FromModel != "fake-claude" || e.Payload.ToModel != "fake-haiku" || e.Payload.Cause != "529" || seen[e.ID] {
				t.Fatalf("event %+v", e)
			}
			seen[e.ID] = true
		case <-timeout:
			t.Fatalf("%d of %d events delivered", len(seen), burst)
		}
	}
	select {
	case e := <-subscription.Events:
		t.Fatalf("extra event %+v", e)
	case <-time.After(100 * time.Millisecond):
	}
	if subscription.Lost.Load() != 0 {
		t.Fatal("events lost")
	}
}

func TestFallbackSecondProviderGetsSanitizedBody(t *testing.T) {
	gw := startFallbackGateway(t, "    fallback: [fake-haiku]\n    detection:\n      email: redact\n", map[string]fakeprovider.Scenario{"fake-claude": fakeprovider.ScenarioOverload}, gatewayOptions{sensitive: true})
	key := gw.createKey("bot").Key.Reveal()
	body := strings.Replace(testkit.AnthropicBody(), "Ola", "Ola "+testkit.EmailID(), 1)
	if response, text := gw.call("/v1/messages", key, body); response.StatusCode != 200 {
		t.Fatalf("%d %s", response.StatusCode, text)
	}
	first, second := gw.lastRequest(), gw.lastRequest()
	for _, r := range []string{string(first.Body), string(second.Body)} {
		if strings.Contains(r, testkit.EmailID()) || !placeholderPattern.MatchString(r) {
			t.Fatalf("a provider received the raw value: %s", r)
		}
	}
	if placeholderPattern.FindString(string(first.Body)) != placeholderPattern.FindString(string(second.Body)) {
		t.Fatal("the attempts were inspected separately")
	}
	gw.results.waitCount(t, 2)
	if n := len(gw.policyEvents(t, events.SensitiveDataDetected)); n != 1 {
		t.Fatalf("inspection events %d", n)
	}
}
