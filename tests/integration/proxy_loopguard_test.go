package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/events"
	"github.com/JoaoVictorVM/comporta/internal/fakeprovider"
	"github.com/JoaoVictorVM/comporta/internal/loopguard"
	"github.com/JoaoVictorVM/comporta/internal/storage"
	"github.com/JoaoVictorVM/comporta/internal/testkit"
)

type loopRig struct {
	t        *testing.T
	gw       *gateway
	clock    *loopguard.FakeClock
	key      string
	upstream *atomic.Int64
}

func startLoopGateway(t *testing.T, scenario fakeprovider.Scenario, edit func(string) string) *loopRig {
	t.Helper()
	upstream := &atomic.Int64{}
	clock := loopguard.NewFakeClock(time.Now())
	fake := fakeprovider.Options{Scenario: scenario, OnRequest: func(fakeprovider.Request) { upstream.Add(1) }}
	gw := startConfiguredGateway(t, fake, gatewayOptions{metered: true, loop: true, loopClock: clock, edit: edit})
	return &loopRig{t: t, gw: gw, clock: clock, key: gw.createKey("bot").Key.Reveal(), upstream: upstream}
}

func (r *loopRig) send(path, body string) (*http.Response, string) {
	r.t.Helper()
	r.clock.Advance(time.Second)
	return r.gw.call(path, r.key, body)
}

func loopCode(t *testing.T, body string) string {
	t.Helper()
	var parsed struct {
		Error struct {
			Code string `json:"comporta_code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("not a JSON error %q", body)
	}
	return parsed.Error.Code
}

func (r *loopRig) loopRecords() []storage.Record {
	r.t.Helper()
	page, err := r.gw.usage.Records(context.Background(), storage.RecordFilter{AppName: "bot", Limit: 1000})
	if err != nil {
		r.t.Fatal(err)
	}
	var cuts []storage.Record
	for _, record := range page.Records {
		if record.Outcome == "loop_cut" {
			cuts = append(cuts, record)
		}
	}
	return cuts
}

func (r *loopRig) loopEvents() []events.Event {
	r.t.Helper()
	page, err := r.gw.usage.ReadEvents(context.Background(), events.Filter{Types: []events.Type{events.LoopCut}})
	if err != nil {
		r.t.Fatal(err)
	}
	return page.Events
}

// TestGrowingContextLoopCut runs an agent that answers the same tool call with
// the same result while its context grows: the first ten similar requests
// reach the provider and the eleventh is cut before it.
func TestGrowingContextLoopCut(t *testing.T) {
	for _, format := range []struct {
		anthropic   bool
		path, model string
	}{{true, "/v1/messages", "fake-claude"}, {false, "/v1/chat/completions", "fake-gpt"}} {
		t.Run(format.model, func(t *testing.T) {
			r := startLoopGateway(t, fakeprovider.ScenarioAgentLoop, nil)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			sub := r.gw.usage.Bus.Subscribe(ctx)
			agent := fakeprovider.NewAgentLoop(format.anthropic, format.model, "agent-user", testkit.Prompt("check the deployment"))
			result := testkit.Prompt(fakeprovider.LoopToolResult)
			response, body := r.send(format.path, string(agent.Body(false)))
			if response.StatusCode != 200 || agent.Advance([]byte(body), result) != nil {
				t.Fatalf("task turn: %d %s", response.StatusCode, body)
			}
			previous := 0
			for similar := 1; similar <= 10; similar++ {
				request := agent.Body(false)
				if len(request) <= previous {
					t.Fatal("the context did not grow")
				}
				previous = len(request)
				response, body := r.send(format.path, string(request))
				if response.StatusCode != 200 {
					t.Fatalf("similar request %d: %d %s", similar, response.StatusCode, body)
				}
				if err := agent.Advance([]byte(body), result); err != nil {
					t.Fatal(err)
				}
			}
			reached := r.upstream.Load()
			response, body = r.send(format.path, string(agent.Body(false)))
			if response.StatusCode != http.StatusTooManyRequests || loopCode(t, body) != "loop_detected" ||
				!strings.Contains(body, "loop detected: 10 similar requests in 30s; requests from this user are paused for 120s") {
				t.Fatalf("eleventh similar request: %d %s", response.StatusCode, body)
			}
			if r.upstream.Load() != reached || reached != 11 {
				t.Fatalf("provider calls: %d before, %d after", reached, r.upstream.Load())
			}
			// Retries during the cooldown keep the one event of the episode.
			for range 3 {
				if response, _ := r.send(format.path, string(agent.Body(false))); response.StatusCode != 429 {
					t.Fatal("retry during the cooldown")
				}
			}
			if cuts := r.loopRecords(); len(cuts) != 4 {
				t.Fatalf("cut records: %d", len(cuts))
			}
			stored := r.loopEvents()
			if len(stored) != 1 || stored[0].Payload.Reason != events.ReasonSimilarRequests || stored[0].Payload.Count != 11 ||
				stored[0].Payload.WindowMS != 30000 || stored[0].Payload.CooldownMS != 120000 || stored[0].UserHash == "" {
				t.Fatalf("episode events: %+v", stored)
			}
			deadline := time.After(5 * time.Second)
			for {
				select {
				case e := <-sub.Events:
					if e.Type != events.LoopCut {
						continue
					}
					if e.ID != stored[0].ID {
						t.Fatalf("notified another event: %+v", e)
					}
				case <-deadline:
					t.Fatal("no loop cut notification")
				}
				break
			}
		})
	}
}

// A tool result that changes every turn is progress, not a loop.
func TestGrowingContextWithChangingResultNotCut(t *testing.T) {
	r := startLoopGateway(t, fakeprovider.ScenarioAgentLoop, nil)
	agent := fakeprovider.NewAgentLoop(false, "fake-gpt", "agent-user", testkit.Prompt("check the deployment"))
	for turn := range 20 {
		response, body := r.send("/v1/chat/completions", string(agent.Body(false)))
		if response.StatusCode != 200 {
			t.Fatalf("turn %d: %d %s", turn, response.StatusCode, body)
		}
		if err := agent.Advance([]byte(body), testkit.Prompt(fmt.Sprintf("status: step %d", turn))); err != nil {
			t.Fatal(err)
		}
	}
}

func rateBody(i int) string {
	return fmt.Sprintf(`{"model":"fake-gpt","user":"busy-user","messages":[{"role":"user","content":%q}]}`, testkit.Prompt(fmt.Sprint("question ", i)))
}

func TestLoopGuardRateCut61(t *testing.T) {
	r := startLoopGateway(t, "", nil)
	for i := range 60 {
		r.clock.Advance(-time.Second + 900*time.Millisecond)
		if response, body := r.send("/v1/chat/completions", rateBody(i)); response.StatusCode != 200 {
			t.Fatalf("request %d: %d %s", i+1, response.StatusCode, body)
		}
	}
	response, body := r.send("/v1/chat/completions", rateBody(60))
	if response.StatusCode != 429 || loopCode(t, body) != "loop_detected" || !strings.Contains(body, "more than 60 requests in 60s") {
		t.Fatalf("61st request: %d %s", response.StatusCode, body)
	}
	if r.upstream.Load() != 60 {
		t.Fatalf("provider calls: %d", r.upstream.Load())
	}
}

func cutUser(t *testing.T, r *loopRig, body string) {
	t.Helper()
	for i := range 10 {
		if response, text := r.send("/v1/chat/completions", body); response.StatusCode != 200 {
			t.Fatalf("similar request %d: %d %s", i+1, response.StatusCode, text)
		}
	}
	if response, text := r.send("/v1/chat/completions", body); response.StatusCode != 429 {
		t.Fatalf("eleventh: %d %s", response.StatusCode, text)
	}
}

func TestLoopCooldownBoundary(t *testing.T) {
	r := startLoopGateway(t, "", nil)
	body := rateBody(0)
	cutUser(t, r, body)
	// send advances one second before each call: the cut started at 11s and
	// lasts until 131s, so the next call lands at 130s.
	r.clock.Advance(118 * time.Second)
	response, text := r.send("/v1/chat/completions", body)
	if response.StatusCode != 429 || response.Header.Get("Retry-After") != "1" || !strings.Contains(text, "paused for 1s") {
		t.Fatalf("last second of the cooldown: %d %v %s", response.StatusCode, response.Header, text)
	}
	before := r.upstream.Load()
	if response, text = r.send("/v1/chat/completions", body); response.StatusCode != 200 {
		t.Fatalf("exactly at the end of the cooldown: %d %s", response.StatusCode, text)
	}
	if r.upstream.Load() != before+1 {
		t.Fatal("traffic did not resume")
	}
}

func TestLoopGuardDisabled(t *testing.T) {
	disable := func(text string) string {
		return strings.Replace(text, "  bot:\n    allowed_models: [fake-claude, fake-gpt]\n", "  bot:\n    allowed_models: [fake-claude, fake-gpt]\n    loop_guard:\n      enabled: false\n", 1)
	}
	r := startLoopGateway(t, "", disable)
	for i := range 70 {
		r.clock.Advance(-900 * time.Millisecond)
		if response, text := r.send("/v1/chat/completions", rateBody(0)); response.StatusCode != 200 {
			t.Fatalf("request %d of a disabled app: %d %s", i+1, response.StatusCode, text)
		}
	}
	if len(r.loopRecords()) != 0 || len(r.loopEvents()) != 0 {
		t.Fatal("a disabled app recorded a cut")
	}
}

func TestLoopStreamingCutBeforeFirstByte(t *testing.T) {
	r := startLoopGateway(t, "", nil)
	stream := strings.Replace(rateBody(0), `"model":"fake-gpt",`, `"model":"fake-gpt","stream":true,`, 1)
	for range 10 {
		r.send("/v1/chat/completions", stream)
	}
	response, body := r.send("/v1/chat/completions", stream)
	if response.StatusCode != 429 || response.Header.Get("Content-Type") != "application/json" || strings.Contains(body, "data:") || response.Header.Get("Retry-After") != "120" {
		t.Fatalf("stream cut: %d %v %s", response.StatusCode, response.Header, body)
	}
}

func TestLoopReloadWithoutRestart(t *testing.T) {
	r := startLoopGateway(t, "", nil)
	body := rateBody(0)
	cutUser(t, r, body)
	r.gw.reload(func(text string) string {
		return strings.Replace(text, "  bot:\n    allowed_models: [fake-claude, fake-gpt]\n", "  bot:\n    allowed_models: [fake-claude, fake-gpt]\n    loop_guard:\n      enabled: false\n", 1)
	})
	if response, text := r.send("/v1/chat/completions", body); response.StatusCode != 200 {
		t.Fatalf("disabled by reload: %d %s", response.StatusCode, text)
	}
	r.gw.reload(func(text string) string {
		return strings.Replace(text, "    loop_guard:\n      enabled: false\n", "    loop_guard:\n      similar_requests: 2\n", 1)
	})
	for range 2 {
		if response, text := r.send("/v1/chat/completions", body); response.StatusCode != 200 {
			t.Fatalf("re-enabled starts empty: %d %s", response.StatusCode, text)
		}
	}
	if response, text := r.send("/v1/chat/completions", body); response.StatusCode != 429 || !strings.Contains(text, "2 similar requests") {
		t.Fatalf("new threshold: %d %s", response.StatusCode, text)
	}
}

func TestLoopRejectDoesNotReserveBudget(t *testing.T) {
	budget := func(text string) string {
		return strings.Replace(text, "  bot:\n    allowed_models: [fake-claude, fake-gpt]\n", "  bot:\n    allowed_models: [fake-claude, fake-gpt]\n    budget:\n      usd_per_day: 100\n", 1)
	}
	r := startLoopGateway(t, "", budget)
	body := rateBody(0)
	cutUser(t, r, body)
	for range 5 {
		r.send("/v1/chat/completions", body)
	}
	r.gw.results.waitCount(t, 10)
	for _, cut := range r.loopRecords() {
		if cut.ReservedCost != 0 || cut.Cost != 0 || cut.RequestCount != 0 || cut.AttemptIndex != 0 {
			t.Fatalf("a cut carries money: %+v", cut)
		}
	}
	if len(r.loopRecords()) != 6 {
		t.Fatalf("cut records: %d", len(r.loopRecords()))
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		rows, err := r.gw.usage.Daily(context.Background(), storage.Filter{AppName: "bot"})
		if err != nil {
			t.Fatal(err)
		}
		var requests int64
		for _, row := range rows {
			if row.Scope == "app" {
				requests += row.Requests
			}
		}
		if requests == 10 {
			break
		}
		if requests > 10 || time.Now().After(deadline) {
			t.Fatalf("daily requests: %d, want only the 10 forwarded", requests)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
