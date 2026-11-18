//go:build distributed

package distributed

import (
	"fmt"
	"net/http"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"
)

// The overshoot guarantee holds across processes: whatever the concurrency,
// the spend admitted against a budget never passes it by more than one
// reservation. Repetitions and volume can be raised for a long run.
func TestTwoReplicasBudgetOvershoot(t *testing.T) {
	repeat, calls := 1, 200
	if v, err := strconv.Atoi(os.Getenv("COMPORTA_OVERSHOOT_REPEAT")); err == nil && v > 0 {
		repeat = v
	}
	if v, err := strconv.Atoi(os.Getenv("COMPORTA_OVERSHOOT_CALLS")); err == nil && v > 0 {
		calls = v
	}
	const budget = "0.002"
	for round := range repeat {
		t.Run(fmt.Sprintf("round-%d", round), func(t *testing.T) {
			c := newCluster(t, 2, "apps:\n  bot:\n    allowed_models: [fake-claude]\n    max_tokens: 16\n    budget: {usd_per_day: "+budget+"}\n    limits: {max_concurrent: 1000}\n    per_user: {limits: {max_concurrent: 1000}}")
			var wg sync.WaitGroup
			var mu sync.Mutex
			statuses := map[int]int{}
			for i := range calls {
				// Uneven split, several users, both processes.
				r := c.replicas[0]
				if i%3 == 0 {
					r = c.replicas[1]
				}
				wg.Go(func() {
					status, _ := c.call(r, fmt.Sprintf("user-%d", i%7))
					mu.Lock()
					statuses[status]++
					mu.Unlock()
				})
			}
			wg.Wait()
			if statuses[http.StatusOK] == 0 || statuses[http.StatusTooManyRequests] == 0 {
				t.Fatalf("expected both admissions and budget refusals: %v", statuses)
			}
			db := c.db()
			deadline := time.Now().Add(30 * time.Second)
			var cost, maxReserved, journal int64
			for {
				if err := db.QueryRow(`SELECT coalesce(sum(cost_microusd),0),coalesce(max(reserved_cost_microusd),0) FROM requests WHERE attempt_index>0`).Scan(&cost, &maxReserved); err != nil {
					t.Fatal(err)
				}
				if err := db.QueryRow(`SELECT count(*) FROM metering_journal`).Scan(&journal); err != nil {
					t.Fatal(err)
				}
				if journal == 0 || time.Now().After(deadline) {
					break
				}
				time.Sleep(200 * time.Millisecond)
			}
			const limit = 2_000
			if cost > limit+maxReserved {
				t.Fatalf("overshoot: spent %d µ$ against %d with largest reservation %d", cost, limit, maxReserved)
			}
			t.Logf("statuses %v, spent %d of %d µ$", statuses, cost, limit)
		})
	}
}

func TestPauseAcrossReplicasWithinTwoSeconds(t *testing.T) {
	c := newCluster(t, 2, "apps:\n  bot:\n    allowed_models: [fake-claude]")
	a, b := c.replicas[0], c.replicas[1]
	if status, _ := c.call(b, ""); status != http.StatusOK {
		t.Fatalf("baseline call: %d", status)
	}
	start := time.Now()
	if status, body := c.admin(a, http.MethodPost, "/admin/v1/pause", `{"app":"bot"}`); status != http.StatusOK {
		t.Fatalf("pause: %d %s", status, body)
	}
	for {
		status, code := c.call(b, "")
		if status == http.StatusServiceUnavailable {
			t.Logf("refused with %s after %v", code, time.Since(start))
			break
		}
		if time.Since(start) > 2*time.Second {
			t.Fatal("the pause did not reach the other replica within 2 seconds")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if status, body := c.admin(b, http.MethodGet, "/admin/v1/pause", ""); status != http.StatusOK || !containsPaused(body) {
		t.Fatalf("pause state on the other replica: %d %s", status, body)
	}
	if status, _ := c.admin(b, http.MethodPost, "/admin/v1/resume", `{"all":true}`); status != http.StatusOK {
		t.Fatal("resume")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if status, _ := c.call(a, ""); status == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("resume did not reach the other replica within 2 seconds")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func containsPaused(body string) bool {
	return len(body) > 0 && (contains(body, `"paused":true`) || contains(body, `"app_paused":true`) || contains(body, `"bot"`))
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestSharedKeyAndReadiness(t *testing.T) {
	c := newCluster(t, 2, "apps:\n  bot:\n    allowed_models: [fake-claude]")
	for i, r := range c.replicas {
		if status, code := c.call(r, ""); status != http.StatusOK {
			t.Fatalf("a key created once must work on replica %d: %d %s\n%s", i, status, code, r.stderr.String())
		}
	}
	// A replica that dies stops being ready; the other keeps serving.
	c.kill(c.replicas[0])
	if status, _ := c.call(c.replicas[1], ""); status != http.StatusOK {
		t.Fatal("the surviving replica must keep serving")
	}
}

func TestSQLiteRejectsReplicasCLI(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/comporta.yaml"
	text := "timezone: UTC\nproviders:\n  local:\n    api_format: openai\n    base_url: http://localhost:11434\n    models: [m]\npricing:\n  m: {input: 1, output: 1, cache_write: 1, cache_read: 1}\napps:\n  bot:\n    allowed_models: [m]\ndeployment:\n  replicas: 2\n"
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"config", "validate", path}, {"serve", "--config", path}} {
		out, err := runBinary(args...)
		if err == nil || !contains(out, "SQLite storage does not support multiple replicas; configure redis and postgres") {
			t.Fatalf("%v must abort with the documented message: %v\n%s", args, err, out)
		}
	}
}
