package privacy

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/JoaoVictorVM/comporta/internal/events"
	"github.com/JoaoVictorVM/comporta/internal/storage"
	"github.com/JoaoVictorVM/comporta/internal/storage/sqlite"
	"github.com/JoaoVictorVM/comporta/internal/testkit"
)

// TestLoopCutCanaryAllOutputs cuts a marked agent loop and scans the refusal,
// the logs, the database and the files for the prompt, the tool result and the
// raw user ID. Only arrival instants and digests ever exist, in memory.
func TestLoopCutCanaryAllOutputs(t *testing.T) {
	run.markCase("process/loop-cut")
	g := startLive(t, "agent-loop", "")
	defer g.stop()
	run.markConstructor("AnthropicBody")
	user := testkit.UserMarker()
	for i := range 10 {
		if status := g.send("loop-allowed", "/v1/messages", g.key, user, testkit.AnthropicBody()); status != http.StatusOK {
			t.Fatalf("similar request %d: status %d", i+1, status)
		}
	}
	for range 3 {
		if status := g.send("loop-cut", "/v1/messages", g.key, user, testkit.AnthropicBody()); status != http.StatusTooManyRequests {
			t.Fatalf("positive control: the loop was not cut, status %d", status)
		}
	}
	reader, err := sqlite.OpenReader(context.Background(), filepath.Join(g.dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	page, err := reader.ReadEvents(context.Background(), events.Filter{Types: []events.Type{events.LoopCut}})
	if err != nil || len(page.Events) != 1 {
		t.Fatalf("positive control: loop event %v %v", page, err)
	}
	records, err := reader.Records(context.Background(), storage.RecordFilter{AppName: "bot", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	cuts := 0
	for _, record := range records.Records {
		if record.Outcome == "loop_cut" {
			cuts++
		}
	}
	if cuts != 3 {
		t.Fatalf("positive control: %d cut records", cuts)
	}
}
