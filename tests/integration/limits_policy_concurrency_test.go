package integration

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/limits"
	"github.com/JoaoVictorVM/comporta/internal/storage"
	"github.com/JoaoVictorVM/comporta/internal/testkit"
)

// TestFiveHundredPolicyAdmissionsRespectHardCap fires 500 simultaneous calls at
// a per-user budget already exceeded by any call, alternating rounds between a
// warn app and a downgrade app. The policy lets calls through the budget, and
// the per-user hard cap must hold: with an upstream that never consumes more
// than it reserved, the real spend never crosses the cap.
func TestFiveHundredPolicyAdmissionsRespectHardCap(t *testing.T) {
	const clients = 500
	const hardCap = 200_000
	perUser := "    limits: {max_concurrent: 2000}\n    budget: {usd_per_day: 100000.00}\n    per_user:\n      limits: {max_concurrent: 2000}\n      budget: {usd_per_day: 0.000001}\n      hard_cap: {usd_per_day: 0.20}\n"
	gw := startPolicyGateway(t, "    on_limit: warn\n"+perUser, gatewayOptions{upstream: fullOutput, edit: func(text string) string {
		return strings.Replace(text, "  other:\n    allowed_models: [fake-gpt]\n", "  other:\n    allowed_models: [fake-gpt, fake-gpt-mini]\n    on_limit: downgrade\n    downgrade_to: fake-gpt-mini\n"+perUser, 1)
	}})
	appKeys := map[string]string{"bot": gw.createKey("bot").Key.Reveal(), "other": gw.createKey("other").Key.Reveal()}
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: clients, DialContext: patientDial}, Timeout: 60 * time.Second}
	n := rounds(t)
	finished := 0
	started := time.Now()
	for round := range n {
		app, want := "bot", "warned"
		if round%2 == 1 {
			app, want = "other", "downgraded"
		}
		user := fmt.Sprintf("%s-policy-%d", testkit.UserMarker(), round)
		gate := make(chan struct{})
		statuses := make([]int, clients)
		bodies := make([]string, clients)
		var wg sync.WaitGroup
		for i := range clients {
			wg.Go(func() {
				request, _ := http.NewRequest(http.MethodPost, gw.proxyURL("/v1/chat/completions"), strings.NewReader(testkit.OpenAIBody()))
				request.Header.Set("Authorization", "Bearer "+appKeys[app])
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("X-Comporta-User", user)
				<-gate
				response, err := client.Do(request)
				if err != nil {
					bodies[i] = err.Error()
					return
				}
				body, _ := io.ReadAll(response.Body)
				response.Body.Close()
				statuses[i], bodies[i] = response.StatusCode, string(body)
			})
		}
		close(gate)
		wg.Wait()
		admitted := 0
		for i, status := range statuses {
			switch {
			case status == 200:
				admitted++
			case status == 429 && strings.Contains(bodies[i], "hard_cap_exceeded"):
			default:
				t.Fatalf("round %d: unexpected answer %d: %s", round, status, bodies[i])
			}
		}
		finished += admitted
		gw.results.waitCount(t, finished)
		hash, err := gw.identity.LookupHash(app, user)
		if err != nil {
			t.Fatal(err)
		}
		policy, _ := limits.PolicyFor(gw.config.Capture(), app)
		var view limits.LimitView
		for _, v := range gw.limits.State(policy, &limits.Subject{App: app, Hash: hash.Hex()}).Limits {
			if v.LimitName == "apps."+app+".per_user.hard_cap.usd_per_day" {
				view = v
			}
		}
		if admitted == 0 || view.Reserved != 0 || view.Used > hardCap || view.Limit != hardCap {
			t.Fatalf("round %d (%s): %d admitted, cap %+v", round, app, admitted, view)
		}
		if err := gw.accounting.Flush(t.Context()); err != nil {
			t.Fatal(err)
		}
		page, err := gw.usage.Records(t.Context(), storage.RecordFilter{AppName: app, UserHash: hash.Hex(), Limit: 1000})
		if err != nil {
			t.Fatal(err)
		}
		sent := 0
		for _, r := range page.Records {
			if r.AttemptIndex == 0 {
				continue
			}
			sent++
			if r.PolicyOutcome != want {
				t.Fatalf("round %d: admitted record without the policy mark: %+v", round, r)
			}
		}
		if sent != admitted {
			t.Fatalf("round %d: %d records for %d admissions", round, sent, admitted)
		}
	}
	t.Logf("%d rounds of %d concurrent policy calls in %s", n, clients, time.Since(started).Round(time.Millisecond))
}
