//go:build distributed

package distributed

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"
)

// clusterReceiver is the generic webhook every replica delivers to.
type clusterReceiver struct {
	*httptest.Server
	mu    sync.Mutex
	calls []clusterCall
	hold  chan struct{}
}

type clusterCall struct {
	delivery string
	trigger  string
	body     []byte
}

func newClusterReceiver(t *testing.T) *clusterReceiver {
	t.Helper()
	rc := &clusterReceiver{}
	rc.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var p struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(body, &p)
		rc.mu.Lock()
		rc.calls = append(rc.calls, clusterCall{delivery: r.Header.Get("X-Comporta-Delivery-ID"), trigger: p.Type, body: body})
		hold := rc.hold
		rc.mu.Unlock()
		if hold != nil {
			select {
			case <-hold:
			case <-r.Context().Done():
				return
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(rc.Close)
	return rc
}

func (rc *clusterReceiver) of(trigger string) []clusterCall {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	var out []clusterCall
	for _, c := range rc.calls {
		if c.trigger == trigger {
			out = append(out, c)
		}
	}
	return out
}

func waitFor(t *testing.T, within time.Duration, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("%s not observed within %s", what, within)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func alertApps(t *testing.T, rc *clusterReceiver, budget string) string {
	t.Helper()
	t.Setenv("COMPORTA_CLUSTER_HOOK", rc.URL+"/hook")
	t.Setenv("COMPORTA_CLUSTER_SECRET", "cluster-alert-signing-secret")
	return "apps:\n  bot:\n    allowed_models: [fake-claude]\n    max_tokens: 16\n    budget: {usd_per_day: " + budget + "}\n" +
		"alerts:\n  thresholds: [50]\n  channels:\n    hook:\n      type: webhook\n      url: ${COMPORTA_CLUSTER_HOOK}\n      signing_secret: ${COMPORTA_CLUSTER_SECRET}\n"
}

// Both replicas evaluate the completions they and the other one wrote; the
// shared unique key leaves one occurrence and one delivery for the channel.
// The repeated prompt may also cut a loop, which is another alert.
func TestTwoReplicasSendOneDeliveryPerChannel(t *testing.T) {
	rc := newClusterReceiver(t)
	c := newCluster(t, 2, alertApps(t, rc, "0.002"))
	for i := 0; i < 200 && len(rc.of("budget_threshold")) == 0; i++ {
		c.call(c.replicas[i%2], fmt.Sprintf("user-%d", i%5))
	}
	waitFor(t, 60*time.Second, "50% alert", func() bool { return len(rc.of("budget_threshold")) > 0 })
	for i := range 10 {
		c.call(c.replicas[i%2], "user-late")
	}
	time.Sleep(3 * time.Second)
	if n := len(rc.of("budget_threshold")); n != 1 {
		t.Fatalf("%d budget deliveries from two replicas, want one", n)
	}
	var occurrences, deliveries int
	db := c.db()
	if err := db.QueryRow(`SELECT count(*) FROM alert_occurrences WHERE trigger='budget_threshold'`).Scan(&occurrences); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM alert_deliveries d JOIN alert_occurrences o ON o.occurrence_id=d.occurrence_id WHERE d.state='delivered' AND o.trigger='budget_threshold'`).Scan(&deliveries); err != nil {
		t.Fatal(err)
	}
	if occurrences != 1 || deliveries != 1 {
		t.Fatalf("occurrences %d delivered %d", occurrences, deliveries)
	}
}

// A replica that dies while its call is in flight leaves the claim to expire;
// a surviving process takes it over and sends the same delivery again.
func TestClaimOwnerCrash(t *testing.T) {
	rc := newClusterReceiver(t)
	rc.hold = make(chan struct{})
	c := newCluster(t, 2, alertApps(t, rc, "0.002"))
	for i := 0; i < 200 && len(rc.of("budget_threshold")) == 0; i++ {
		c.call(c.replicas[i%2], fmt.Sprintf("user-%d", i%5))
	}
	waitFor(t, 60*time.Second, "first call", func() bool { return len(rc.of("budget_threshold")) == 1 })
	for _, r := range c.replicas {
		c.kill(r)
	}
	rc.mu.Lock()
	close(rc.hold)
	rc.hold = nil
	rc.mu.Unlock()
	c.start(c.replicas[1])
	waitFor(t, 90*time.Second, "takeover", func() bool { return len(rc.of("budget_threshold")) == 2 })
	calls := rc.of("budget_threshold")
	if calls[0].delivery != calls[1].delivery || string(calls[0].body) != string(calls[1].body) {
		t.Fatal("the takeover sent a different delivery")
	}
}

// A rejected reload on one replica is one alert for the cluster, delivered by
// whichever replica claims it.
func TestReloadAcrossReplicas(t *testing.T) {
	rc := newClusterReceiver(t)
	c := newCluster(t, 2, alertApps(t, rc, "1.00"))
	data, err := os.ReadFile(c.replicas[0].path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.replicas[0].path, append(data, []byte("telemetry:\n  otel_enabled: maybe\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 60*time.Second, "failed reload alert", func() bool { return len(rc.of("config_reload_failed")) == 1 })
	time.Sleep(3 * time.Second)
	if n := len(rc.of("config_reload_failed")); n != 1 {
		t.Fatalf("%d reload alerts", n)
	}
}
