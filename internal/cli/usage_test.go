package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/storage"
	"github.com/JoaoVictorVM/comporta/internal/storage/sqlite"
)

func TestUsageSinceSevenCalendarDaysAndAppScope(t *testing.T) {
	path := writeConfig(t, validConfig)
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(filepath.Dir(path), "data"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	n := int64(10)
	for i, days := range []int{0, 6, 7} {
		at := now.AddDate(0, 0, -days)
		r := storage.Record{RequestID: fmt.Sprintf("%032x", i+1), AttemptIndex: 1, StartedAt: at, FinishedAt: at, AppName: "bot", UserHash: strings.Repeat("1", 32), Operation: "generation", Outcome: "ok", Input: &n, Cost: 49, ReservedCost: 100, RequestCount: 1, Day: at.Format("2006-01-02"), Timezone: "UTC"}
		if err = store.Admit(ctx, r); err != nil {
			t.Fatal(err)
		}
		if err = store.Commit(ctx, []storage.Record{r}); err != nil {
			t.Fatal(err)
		}
	}
	code, out, stderr := run(t, "usage", "--app", "bot", "--since", "7d", "--config", path)
	if code != 0 || stderr != "" || !strings.Contains(out, "total - 2 20 0.000098") || strings.Contains(out, strings.Repeat("1", 32)) {
		t.Fatalf("usage exit=%d output=%q stderr=%q", code, out, stderr)
	}
	// Removing an app does not remove the historical timezone or its totals.
	removed := strings.Replace(validConfig, "apps:\n  bot:\n    allowed_models: [model-a]\n", "apps: {}\n", 1)
	if err = os.WriteFile(path, []byte(removed), 0600); err != nil {
		t.Fatal(err)
	}
	code, out, stderr = run(t, "usage", "--app", "bot", "--since", "7d", "--config", path)
	if code != 0 || !strings.Contains(out, "total - 2 20 0.000098") {
		t.Fatalf("removed app history: %d %q %q", code, out, stderr)
	}
	code, out, stderr = run(t, "usage", "--app", "removed", "--config", path)
	if code != 0 || !strings.Contains(out, "total - 0 0 0.000000") {
		t.Fatalf("empty history: %d %q %q", code, out, stderr)
	}
	for _, since := range []string{"0d", "-1d", "+7d", "1h", "1.5d", "7dd"} {
		if code, _, _ := run(t, "usage", "--app", "bot", "--since", since, "--config", path); code != 1 {
			t.Fatalf("accepted %q", since)
		}
	}
}

func TestPricingCheckDoesNotNeedConfigurationOrDatabase(t *testing.T) {
	code, out, stderr := run(t, "pricing", "check", "--config", filepath.Join(t.TempDir(), "absent.yaml"))
	if code != 0 || !strings.Contains(out, "estimated cost") || stderr != "" {
		t.Fatalf("%d %q %q", code, out, stderr)
	}
}

func TestUsageValidatesEmbeddedDowngradeWithoutSecrets(t *testing.T) {
	text := strings.ReplaceAll(validConfig, "model-a", "claude-haiku-4-5")
	text = strings.Replace(text, "  bot:\n", "  bot:\n    on_limit: downgrade\n    downgrade_to: claude-haiku-4-5\n    budget: {usd_per_day: 1}\n", 1)
	path := writeConfig(t, text)
	store, err := sqlite.Open(context.Background(), filepath.Join(filepath.Dir(path), "data"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	code, out, stderr := run(t, "usage", "--app", "bot", "--config", path)
	if code != 0 || !strings.Contains(out, "total - 0 0 0.000000") {
		t.Fatalf("valid embedded downgrade config rejected: %d %q %q", code, out, stderr)
	}
}
