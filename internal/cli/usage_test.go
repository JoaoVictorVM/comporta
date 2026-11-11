package cli

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/storage"
	"github.com/JoaoVictorVM/comporta/internal/storage/sqlite"
)

func TestUsageViaAdminAPI(t *testing.T) {
	api := newFakeAPI(t)
	api.response = func(string, map[string]any) string {
		return `{"reference_at":"2026-11-08T15:00:00Z","materialization_delay_seconds":2,"rows":[{"app":"bot","day":"2026-11-08","timezone":"UTC","requests":"2","tokens":"20","cost_microusd":"98","usage_missing":"0"}],"totals":{"requests":"2","tokens":"20","cost_microusd":"98"},"next_cursor":null}`
	}
	t.Setenv(tokenEnvironment, cliToken)
	code, out, stderr := run(t, "usage", "--app", "bot", "--since", "7d", "--user", "user_42", "--admin-url", api.server.URL)
	if code != 0 || stderr != "" {
		t.Fatalf("%d %q %q", code, out, stderr)
	}
	want := "Reference: 2026-11-08T15:00:00Z; pending completions may take 2 seconds.\nday timezone requests tokens estimated_cost_usd\n2026-11-08 UTC 2 20 0.000098\ntotal - 2 20 0.000098\n"
	if out != want {
		t.Fatalf("output %q", out)
	}
	body := api.bodies[0]
	if body["app"] != "bot" || body["since"] != "7d" || body["user_id"] != "user_42" || api.paths[0] != "POST /admin/v1/usage/query" {
		t.Fatalf("request %v", body)
	}
	code, out, _ = run(t, "usage", "--json", "--admin-url", api.server.URL)
	if code != 0 || !strings.Contains(out, `"next_cursor": null`) || !strings.Contains(out, `"cost_microusd": "98"`) {
		t.Fatalf("json %d %q", code, out)
	}
	if _, ok := api.bodies[1]["app"]; ok {
		t.Fatal("no --app must query every app")
	}
}

func TestUsagePeriodLimit366(t *testing.T) {
	api := newFakeAPI(t)
	t.Setenv(tokenEnvironment, cliToken)
	for _, args := range [][]string{
		{"--since", "367d"}, {"--since", "0d"}, {"--since", "-1d"}, {"--since", "1h"}, {"--since", "1.5d"}, {"--since", "7dd"},
		{"--from", "2025-01-01", "--through", "2026-01-02"}, {"--from", "2026-01-02", "--through", "2026-01-01"},
		{"--from", "2026-01-01"}, {"--from", "2026-1-1", "--through", "2026-01-02"},
		{"--since", "7d", "--from", "2026-01-01", "--through", "2026-01-02"},
	} {
		if code, _, _ := run(t, append([]string{"usage", "--admin-url", api.server.URL}, args...)...); code != 1 {
			t.Fatalf("%v accepted", args)
		}
	}
	if len(api.paths) != 0 {
		t.Fatal("an invalid period reached the gateway")
	}
	api.response = func(string, map[string]any) string {
		return `{"reference_at":"2026-11-08T15:00:00Z","materialization_delay_seconds":2,"rows":[],"totals":{"requests":"0","tokens":"0","cost_microusd":"0"},"next_cursor":null}`
	}
	for _, args := range [][]string{{"--since", "366d"}, {"--from", "2025-01-01", "--through", "2026-01-01"}} {
		if code, _, stderr := run(t, append([]string{"usage", "--admin-url", api.server.URL}, args...)...); code != 0 {
			t.Fatalf("%v: %s", args, stderr)
		}
	}
}

func TestUsageUserAndAnonymousExclusive(t *testing.T) {
	api := newFakeAPI(t)
	t.Setenv(tokenEnvironment, cliToken)
	if code, _, stderr := run(t, "usage", "--user", "u1", "--anonymous", "--admin-url", api.server.URL); code != 1 || !strings.Contains(stderr, "mutually exclusive") {
		t.Fatalf("%d %q", code, stderr)
	}
	if len(api.paths) != 0 {
		t.Fatal("the call was sent")
	}
}

func TestUsagePaginationTotals(t *testing.T) {
	api := newFakeAPI(t)
	t.Setenv(tokenEnvironment, cliToken)
	api.response = func(_ string, body map[string]any) string {
		row := func(day string) string {
			return `{"app":"bot","day":"` + day + `","timezone":"UTC","requests":"1","tokens":"10","cost_microusd":"1000000","usage_missing":"0"}`
		}
		totals := `"totals":{"requests":"2","tokens":"20","cost_microusd":"2000000"}`
		if body["cursor"] == nil {
			return `{"reference_at":"2026-11-08T15:00:00Z","materialization_delay_seconds":2,"rows":[` + row("2026-11-07") + `],` + totals + `,"next_cursor":"c1"}`
		}
		return `{"reference_at":"2026-11-08T15:00:00Z","materialization_delay_seconds":2,"rows":[` + row("2026-11-08") + `],` + totals + `,"next_cursor":null}`
	}
	code, out, stderr := run(t, "usage", "--admin-url", api.server.URL)
	if code != 0 {
		t.Fatal(stderr)
	}
	if len(api.bodies) != 2 || api.bodies[1]["cursor"] != "c1" {
		t.Fatalf("pages %v", api.bodies)
	}
	if !strings.Contains(out, "bot 2026-11-07 UTC 1 10 1.000000\nbot 2026-11-08 UTC 1 10 1.000000\ntotal - - 2 20 2.000000\n") {
		t.Fatalf("output %q", out)
	}
}

func TestUsageNoOfflineFallback(t *testing.T) {
	t.Setenv(tokenEnvironment, cliToken)
	port := closedPort(t)
	text := strings.Replace(validConfig, "apps:", "listeners:\n  admin: {port: "+strconv.Itoa(port)+"}\napps:", 1)
	path := writeConfig(t, text)
	store, err := sqlite.Open(context.Background(), filepath.Join(filepath.Dir(path), "data"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	n := int64(10)
	r := storage.Record{RequestID: fmt.Sprintf("%032x", 1), AttemptIndex: 1, StartedAt: now, FinishedAt: now, AppName: "bot", UserHash: strings.Repeat("1", 32), Operation: "generation", Outcome: "ok", Input: &n, Cost: 49, ReservedCost: 100, RequestCount: 1, Day: now.Format(time.DateOnly), Timezone: "UTC"}
	if err := store.Admit(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if err := store.Commit(context.Background(), []storage.Record{r}); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()
	code, out, stderr := run(t, "usage", "--app", "bot", "--config", path)
	if code != 1 || out != "" || !strings.Contains(stderr, "cannot reach Comporta admin API at 127.0.0.1:"+strconv.Itoa(port)) {
		t.Fatalf("usage must not fall back to the local database: %d %q %q", code, out, stderr)
	}
	t.Setenv(tokenEnvironment, "")
	if code, _, stderr := run(t, "usage", "--app", "bot", "--config", path); code != 1 || !strings.Contains(stderr, tokenEnvironment) {
		t.Fatalf("usage without a token: %d %q", code, stderr)
	}
}

func TestPricingCheckDoesNotNeedConfigurationOrDatabase(t *testing.T) {
	code, out, stderr := run(t, "pricing", "check", "--config", filepath.Join(t.TempDir(), "absent.yaml"))
	if code != 0 || !strings.Contains(out, "estimated cost") || stderr != "" {
		t.Fatalf("%d %q %q", code, out, stderr)
	}
}

func TestUsageFindsTheConfiguredListenerWithoutSecrets(t *testing.T) {
	text := strings.ReplaceAll(validConfig, "model-a", "claude-haiku-4-5")
	text = strings.Replace(text, "  bot:\n", "  bot:\n    on_limit: downgrade\n    downgrade_to: claude-haiku-4-5\n    budget: {usd_per_day: 1}\n", 1)
	port := closedPort(t)
	text = strings.Replace(text, "apps:", "listeners:\n  admin: {port: "+strconv.Itoa(port)+"}\napps:", 1)
	t.Setenv(tokenEnvironment, cliToken)
	code, _, stderr := run(t, "usage", "--app", "bot", "--config", writeConfig(t, text))
	if code != 1 || !strings.Contains(stderr, "127.0.0.1:"+strconv.Itoa(port)) {
		t.Fatalf("the configured admin port was not used: %d %q", code, stderr)
	}
}
