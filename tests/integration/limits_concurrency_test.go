package integration

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/storage"
	"github.com/JoaoVictorVM/comporta/internal/testkit"
)

// fullOutput answers chat completions with usage that fills the output cap, so
// each call consumes close to its reservation without exceeding it.
func fullOutput(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			next.ServeHTTP(w, r)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","object":"chat.completion","created":0,"model":"fake-gpt","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":4096,"total_tokens":4106}}`)
	})
}

func rounds(t *testing.T) int {
	if value := os.Getenv("COMPORTA_LIMITS_ROUNDS"); value != "" {
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 {
			t.Fatal("COMPORTA_LIMITS_ROUNDS must be a positive integer")
		}
		return n
	}
	if testing.Short() || raceEnabled {
		return 5
	}
	return 100
}

// TestFiveHundredConcurrentBudgetOneHundredRuns fires 500 simultaneous calls at
// one per-user budget, in consecutive rounds with an isolated user each, and
// checks that the real spend never exceeds the budget by more than the largest
// single reservation.
func TestFiveHundredConcurrentBudgetOneHundredRuns(t *testing.T) {
	const clients = 500
	const budget = 200_000
	gw := startLimitedGateway(t, "    limits: {max_concurrent: 2000}\n    budget: {usd_per_day: 100000.00}\n    per_user:\n      limits: {max_concurrent: 2000}\n      budget: {usd_per_day: 0.20}\n",
		gatewayOptions{upstream: fullOutput})
	key := gw.createKey("bot").Key.Reveal()
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: clients, DialContext: patientDial}, Timeout: 60 * time.Second}
	n := rounds(t)
	finished := 0
	worst := int64(-1 << 62)
	started := time.Now()
	for round := range n {
		user := fmt.Sprintf("%s-round-%d", testkit.UserMarker(), round)
		gate := make(chan struct{})
		statuses := make([]int, clients)
		errs := make([]error, clients)
		var wg sync.WaitGroup
		for i := range clients {
			wg.Go(func() {
				request, _ := http.NewRequest(http.MethodPost, gw.proxyURL("/v1/chat/completions"), strings.NewReader(testkit.OpenAIBody()))
				request.Header.Set("Authorization", "Bearer "+key)
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("X-Comporta-User", user)
				<-gate
				response, err := client.Do(request)
				if err != nil {
					errs[i] = err
					return
				}
				body, _ := io.ReadAll(response.Body)
				response.Body.Close()
				statuses[i] = response.StatusCode
				if response.StatusCode != 200 && response.StatusCode != 429 {
					errs[i] = fmt.Errorf("%s", body)
				}
			})
		}
		close(gate)
		wg.Wait()
		admitted := 0
		for _, status := range statuses {
			switch status {
			case 200:
				admitted++
			case 429:
			default:
				t.Fatalf("round %d: unexpected status %d: %v", round, status, errs[slicesIndex(statuses, status)])
			}
		}
		// Each admitted call finishes its accounting right after its answer is
		// written, so wait for every result before comparing balances.
		finished += admitted
		gw.results.waitCount(t, finished)
		subject := gw.userSubject(user)
		view := gw.limitView(subject, "apps.bot.per_user.budget.usd_per_day")
		if view.Reserved != 0 {
			t.Fatalf("round %d: reservations left after every call ended: %+v", round, view)
		}
		hash := subject.Hash
		if spent := gw.dailyCost(storage.Filter{UserHash: hash}); spent != view.Used {
			t.Fatalf("round %d: budget %d disagrees with usage %d", round, view.Used, spent)
		}
		largest := maxReservation(t, gw, hash)
		overshoot := view.Used - budget
		worst = max(worst, overshoot)
		if admitted == 0 || overshoot > largest {
			t.Fatalf("round %d: spent %d over a budget of %d with largest reservation %d (%d admitted)", round, view.Used, budget, largest, admitted)
		}
	}
	t.Logf("%d rounds of %d concurrent calls in %s; worst overshoot %d microdollars", n, clients, time.Since(started).Round(time.Millisecond), worst)
}

func maxReservation(t *testing.T, gw *gateway, hash string) int64 {
	t.Helper()
	page, err := gw.usage.Records(t.Context(), storage.RecordFilter{AppName: "bot", UserHash: hash, Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	largest := int64(0)
	for _, r := range page.Records {
		largest = max(largest, int64(r.ReservedCost))
	}
	return largest
}

// TestMultipleUnderestimatedInFlightExposeGuaranteeBoundary documents the limit
// of the guarantee: when several calls in flight all consume more than they
// reserved, the excess adds up beyond a single reservation. The real cost is
// still charged in full and the budget then refuses new calls.
func TestMultipleUnderestimatedInFlightExposeGuaranteeBoundary(t *testing.T) {
	entered, release := make(chan struct{}, 8), make(chan struct{})
	expensive := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			entered <- struct{}{}
			<-release
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"x","object":"chat.completion","created":0,"model":"fake-gpt","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":20000,"total_tokens":20010}}`)
		})
	}
	gw := startLimitedGateway(t, "    budget: {usd_per_day: 0.08}\n", gatewayOptions{upstream: expensive})
	key := gw.createKey("bot").Key.Reveal()
	var wg sync.WaitGroup
	for range 3 {
		wg.Go(func() { gw.call("/v1/chat/completions", key, testkit.OpenAIBody()) })
	}
	for range 3 {
		<-entered
	}
	if response, _ := gw.call("/v1/chat/completions", key, testkit.OpenAIBody()); response.StatusCode != 429 {
		t.Fatal("a fourth reservation fit a budget sized for three")
	}
	close(release)
	wg.Wait()
	gw.results.waitCount(t, 3)
	v := gw.limitView(nil, "apps.bot.budget.usd_per_day")
	largest := maxReservation(t, gw, "")
	if overshoot := v.Used - v.Limit; overshoot <= largest {
		t.Fatalf("the adversarial case did not exceed one reservation: spent %d limit %d largest %d", v.Used, v.Limit, largest)
	}
	if response, body := gw.call("/v1/chat/completions", key, testkit.OpenAIBody()); response.StatusCode != 429 || !strings.Contains(body, "budget_exceeded") {
		t.Fatalf("overspent budget still admits: %d %s", response.StatusCode, body)
	}
}

func slicesIndex(values []int, want int) int {
	for i, v := range values {
		if v == want {
			return i
		}
	}
	return 0
}

// patientDial retries failed dials for up to a minute. 500 simultaneous dials
// overflow the listen backlog of some systems, Windows among them, and tens of
// thousands of one-request connections can exhaust ephemeral ports while they
// wait in TIME_WAIT; both are the operating system's, before the gateway sees
// the request.
func patientDial(ctx context.Context, network, address string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	deadline := time.Now().Add(time.Minute)
	for attempt := 0; ; attempt++ {
		conn, err := dialer.DialContext(ctx, network, address)
		if err == nil || time.Now().After(deadline) || ctx.Err() != nil {
			return conn, err
		}
		time.Sleep(time.Duration(5+attempt%50) * time.Millisecond)
	}
}
