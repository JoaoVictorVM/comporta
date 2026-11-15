package privacy

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/storage"
	"github.com/JoaoVictorVM/comporta/internal/storage/sqlite"
	"github.com/JoaoVictorVM/comporta/internal/testkit"
)

// TestPolicyPathsDoNotPersistContentOrRawIdentity sends marked prompts and user
// identifiers through downgrade, warn, the hard cap and the allowlist, then
// scans every answer, log, file and database artifact the processes left.
func TestPolicyPathsDoNotPersistContentOrRawIdentity(t *testing.T) {
	run.markCase("process/limit-policies")
	g := startLiveWith(t, "success", "", func(text string) string {
		text = strings.Replace(text, "models: [fake-claude]", "models: [fake-claude, fake-haiku]", 1)
		text = strings.Replace(text, "pricing:\n", "pricing:\n  fake-haiku: {input: 1, output: 5, cache_write: 1.25, cache_read: 0.1}\n", 1)
		text = strings.Replace(text, "  bot:\n    allowed_models: [fake-claude, fake-gpt]\n",
			"  bot:\n    allowed_models: [fake-claude, fake-gpt]\n    on_limit: warn\n    budget: {usd_per_day: 0.000001}\n", 1)
		return strings.Replace(text, "  limited:\n    allowed_models: [fake-claude]\n    budget: {usd_per_day: 1.00}\n    per_user:\n      limits: {requests_per_minute: 1}\n",
			"  limited:\n    allowed_models: [fake-claude, fake-haiku]\n    on_limit: downgrade\n    downgrade_to: fake-haiku\n    budget: {usd_per_day: 0.000001}\n    hard_cap: {usd_per_day: 0.0003}\n", 1)
	})
	defer g.stop()
	run.markConstructor("AnthropicBody")
	run.markConstructor("AnthropicStreamBody")
	user := testkit.UserMarker()
	if status := g.send("policy-warned", "/v1/messages", g.key, user, testkit.AnthropicBody()); status != http.StatusOK {
		t.Fatalf("warned: status %d", status)
	}
	if status := g.send("policy-warned-stream", "/v1/messages", g.key, user, testkit.AnthropicStreamBody()); status != http.StatusOK {
		t.Fatalf("warned stream: status %d", status)
	}
	forbidden := strings.Replace(testkit.AnthropicBody(), `"model":"fake-claude"`, `"model":"fake-haiku"`, 1)
	if status := g.send("policy-forbidden", "/v1/messages", g.key, user, forbidden); status != http.StatusForbidden {
		t.Fatalf("positive control: the allowlist did not refuse, status %d", status)
	}
	downgraded, capped := 0, false
	for i := range 50 {
		// Distinct prompts keep the loop guard out of the way of the hard cap.
		body := strings.Replace(testkit.AnthropicBody(), "Ola", fmt.Sprintf("Ola %d", i), 1)
		status := g.send("policy-downgrade", "/v1/messages", g.limitedKey, user, body)
		if status == http.StatusTooManyRequests {
			capped = true
			break
		}
		if status != http.StatusOK {
			t.Fatalf("downgrade: status %d", status)
		}
		downgraded++
	}
	if downgraded == 0 || !capped {
		t.Fatalf("positive control: %d downgraded, hard cap reached %v", downgraded, capped)
	}
	// Two warned requests, the downgraded ones and the refusal at the cap.
	waitForRecords(t, g, 2+downgraded+1)
	g.stop()
	db, err := sql.Open("sqlite", filepath.Join(g.dir, "data", "usage.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, query := range []string{
		`SELECT count(*) FROM events WHERE type='warned'`,
		`SELECT count(*) FROM events WHERE type='downgraded'`,
		`SELECT count(*) FROM requests WHERE json_extract(record_json,'$.PolicyOutcome')='downgraded'`,
		`SELECT count(*) FROM requests WHERE json_extract(record_json,'$.PolicyOutcome')='blocked'`,
	} {
		var n int
		if err = db.QueryRow(query).Scan(&n); err != nil || n == 0 {
			t.Fatalf("positive control: %s found nothing (%v)", query, err)
		}
	}
}

// waitForRecords waits until the usage worker materialized the records of both
// policy apps, so the scan covers them.
func waitForRecords(t *testing.T, g *liveGateway, want int) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		reader, err := sqlite.OpenReader(context.Background(), filepath.Join(g.dir, "data"))
		if err != nil {
			t.Fatal(err)
		}
		total := 0
		for _, app := range []string{"bot", "limited"} {
			page, err := reader.Records(context.Background(), storage.RecordFilter{AppName: app, Limit: 1000})
			if err != nil {
				reader.Close()
				t.Fatal(err)
			}
			total += len(page.Records)
		}
		reader.Close()
		if total >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("positive control: %d of %d records materialized", total, want)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
