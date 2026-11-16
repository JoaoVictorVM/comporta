package privacy

import (
	"database/sql"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JoaoVictorVM/comporta/internal/testkit"
)

func withCache(settings string) func(string) string {
	return func(text string) string {
		anchor := "  bot:\n    allowed_models: [fake-claude, fake-gpt]\n"
		return strings.Replace(text, anchor, anchor+"    cache:\n      enabled: true\n"+settings, 1)
	}
}

func cacheable(body string) string { return testkit.WithField(body, `"temperature":0`) }

func countRows(t *testing.T, g *liveGateway, query string) int {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(g.dir, "data", "usage.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(query).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestCacheCanaryPersistedChannels fills and hits the cache with marked
// prompts, JSON and stream, then scans the answers, logs, files and database:
// cached content only ever goes back to the client that is entitled to it.
func TestCacheCanaryPersistedChannels(t *testing.T) {
	run.markCase("process/response-cache")
	g := startLiveWith(t, "success", "", withCache(""))
	defer g.stop()
	run.markConstructor("AnthropicBody")
	run.markConstructor("AnthropicStreamBody")
	user := testkit.UserMarker()
	for _, body := range []string{cacheable(testkit.AnthropicBody()), cacheable(testkit.AnthropicStreamBody())} {
		for range 2 {
			if status := g.send("cache", "/v1/messages", g.key, user, body); status != http.StatusOK {
				t.Fatalf("status %d", status)
			}
		}
	}
	waitForRecords(t, g, 4)
	g.stop()
	if n := countRows(t, g, `SELECT count(*) FROM requests WHERE outcome='cached' AND cost_microusd=0`); n != 2 {
		t.Fatalf("positive control: %d cached records", n)
	}
	if n := countRows(t, g, `SELECT count(*) FROM events WHERE type='cached' AND user_hash IS NULL`); n != 2 {
		t.Fatalf("positive control: %d cached events", n)
	}
}

// TestSharedCacheDoesNotPersistIdentity shares entries across users: each hit
// is recorded under the pseudonym of the user who asked, and no raw identifier
// of either user is kept anywhere.
func TestSharedCacheDoesNotPersistIdentity(t *testing.T) {
	run.markCase("process/response-cache-shared")
	g := startLiveWith(t, "success", "", withCache("      shared_across_users: true\n"))
	defer g.stop()
	run.markConstructor("AnthropicBody")
	body := cacheable(testkit.AnthropicBody())
	for _, user := range []string{testkit.UserMarker(), testkit.UserMarker() + "-b"} {
		if status := g.send("cache-shared", "/v1/messages", g.key, user, body); status != http.StatusOK {
			t.Fatalf("status %d", status)
		}
	}
	waitForRecords(t, g, 2)
	g.stop()
	if n := countRows(t, g, `SELECT count(DISTINCT user_hash) FROM requests WHERE app_name='bot'`); n != 2 {
		t.Fatalf("positive control: %d pseudonyms", n)
	}
	if n := countRows(t, g, `SELECT count(*) FROM requests WHERE outcome='cached'`); n != 1 {
		t.Fatalf("positive control: %d shared hits", n)
	}
}

// TestCacheErrorsAndPanicDoNotLeak breaks a stream the cache was capturing:
// the error answer, the logs and the database hold no content, and nothing of
// the partial stream is kept for a later request.
func TestCacheErrorsAndPanicDoNotLeak(t *testing.T) {
	run.markCase("process/response-cache-errors")
	g := startLiveWith(t, "disconnect-midstream", "", withCache(""))
	defer g.stop()
	run.markConstructor("AnthropicStreamBody")
	body := cacheable(testkit.AnthropicStreamBody())
	for range 2 {
		g.send("cache-error", "/v1/messages", g.key, testkit.UserMarker(), body)
	}
	waitForRecords(t, g, 2)
	g.stop()
	if n := countRows(t, g, `SELECT count(*) FROM requests WHERE outcome='cached'`); n != 0 {
		t.Fatalf("a broken stream was replayed: %d", n)
	}
}
