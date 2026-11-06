package config

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestCapturePinsGeneration(t *testing.T) {
	h := newHarness(t, baseYAML)
	inFlight := h.manager.Capture()
	h.save(t, replaced(t, baseYAML, "usd_per_day: 2.00", "usd_per_day: 5.00"))

	app, _ := inFlight.App("bot")
	if inFlight.Generation() != 1 || app.Budget.USDPerDay != 2_000_000 {
		t.Fatalf("an operation already started must keep its snapshot: gen %d, budget %d", inFlight.Generation(), app.Budget.USDPerDay)
	}
	if h.manager.Capture().Generation() != 2 {
		t.Fatal("a new operation must see the new generation")
	}
}

func TestAccessorsCannotMutateSnapshot(t *testing.T) {
	text := replaced(t, baseYAML, "  bot:\n", "  bot:\n    fallback: [model-b]\n") +
		"alerts:\n  thresholds: [50]\n  channels:\n    ops: {type: slack, url: \"${HOOK_URL}\"}\n" +
		"listeners: {allowed_hosts: [gateway.internal]}\n"
	h := newHarness(t, text, func(o *ManagerOptions) {
		o.Options.Environment = NewEnvironment(map[string]string{"ANTHROPIC_API_KEY": testAPIKey, "HOOK_URL": "https://hooks.example/x"})
	})
	snapshot := h.manager.Capture()

	provider, _ := snapshot.Provider("anthropic")
	provider.Models[0] = "tampered"
	app, _ := snapshot.App("bot")
	app.AllowedModels[0] = "tampered"
	app.Fallback[0] = "tampered"
	listeners := snapshot.Listeners()
	listeners.AllowedHosts[0] = "tampered"
	alerts := snapshot.Alerts()
	alerts.Thresholds[0] = -1
	alerts.Channels["ops"] = AlertChannel{}
	delete(alerts.Channels, "ops")
	names := snapshot.AppNames()
	names[0] = "tampered"

	freshProvider, _ := snapshot.Provider("anthropic")
	freshApp, _ := snapshot.App("bot")
	freshAlerts := snapshot.Alerts()
	switch {
	case freshProvider.Models[0] == "tampered":
		t.Error("provider models are shared")
	case freshApp.AllowedModels[0] == "tampered" || freshApp.Fallback[0] == "tampered":
		t.Error("app lists are shared")
	case snapshot.Listeners().AllowedHosts[0] == "tampered":
		t.Error("allowed hosts are shared")
	case freshAlerts.Thresholds[0] == -1 || freshAlerts.Channels["ops"].Type != AlertSlack:
		t.Error("alert settings are shared")
	case snapshot.AppNames()[0] == "tampered":
		t.Error("app names are shared")
	}
}

func TestSnapshotExposesConsumerContracts(t *testing.T) {
	h := newHarness(t, baseYAML)
	snapshot := h.manager.Capture()

	if owner, ok := snapshot.ProviderForModel("model-b"); !ok || owner != "anthropic" {
		t.Fatalf("model owner: %s %v", owner, ok)
	}
	if key, ok := snapshot.ProviderAPIKey("anthropic"); !ok || key.Reveal() != testAPIKey {
		t.Fatal("provider key must be available to the transport")
	}
	if _, ok := snapshot.ProviderAPIKey("local"); ok {
		t.Fatal("a local provider without a key must have none")
	}
	if price, ok := snapshot.Price("model-b"); !ok || price.CacheWrite != 1_250_000 {
		t.Fatalf("price: %+v %v", price, ok)
	}
	if strings.Join(snapshot.ProviderNames(), ",") != "anthropic,local" {
		t.Fatalf("provider names: %v", snapshot.ProviderNames())
	}
	if snapshot.Listeners().Proxy.Port != 8080 || snapshot.Timezone() != "UTC" || snapshot.FailMode() != FailClosed {
		t.Fatal("root accessors")
	}
	if snapshot.DataDir() == "" || snapshot.Cache().MaxBytes == 0 || !snapshot.Telemetry().MetricsEnabled {
		t.Fatal("default accessors")
	}
}

func TestConcurrentCaptureAndReload(t *testing.T) {
	h := newHarness(t, baseYAML)
	stop := make(chan struct{})
	var readers sync.WaitGroup
	for range 8 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				snapshot := h.manager.Capture()
				// Within one captured snapshot, generation and content must match.
				app, ok := snapshot.App("bot")
				if !ok {
					t.Error("app missing from a published snapshot")
					return
				}
				want := Microusd(snapshot.Generation()+1) * microsPerUSD
				if snapshot.Generation() > 1 && app.Budget.USDPerDay != want {
					t.Errorf("generation %d has budget %d", snapshot.Generation(), app.Budget.USDPerDay)
					return
				}
				_ = h.manager.Status()
				_ = snapshot.EffectiveYAML()
			}
		}()
	}
	for generation := 2; generation <= 40; generation++ {
		text := replaced(t, baseYAML, "usd_per_day: 2.00", fmt.Sprintf("usd_per_day: %d.00", generation+1))
		writeFile(t, h.path, text)
		if status := h.manager.Reload(context.Background()); status.ActiveGeneration != int64(generation) {
			t.Errorf("reload %d: %+v", generation, status)
			break
		}
	}
	close(stop)
	readers.Wait()
}
