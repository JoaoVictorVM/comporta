package integration

import (
	"context"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/fakeprovider"
	"github.com/JoaoVictorVM/comporta/internal/testkit"
)

func otelAt(endpoint string) string {
	return "telemetry:\n  otel_enabled: true\n  otel_endpoint: " + endpoint + "\n"
}

// setTelemetry replaces the telemetry block at the end of the configuration.
func setTelemetry(block string) func(string) string {
	return func(text string) string {
		if i := strings.Index(text, "telemetry:\n"); i >= 0 {
			text = text[:i]
		}
		return text + block
	}
}

func TestConfigControlsMetricsAndTracing(t *testing.T) {
	options := gatewayOptions{metered: true}
	spans := observed(&options)
	gw := startConfiguredGateway(t, fakeprovider.Options{}, options)
	key := gw.createKey("bot").Key.Reveal()
	gw.call("/v1/messages", key, testkit.AnthropicBody())
	gw.waitMetric(t, okClaude, 1)
	if len(spans.created()) != 0 {
		t.Fatalf("tracing is off by default but exporters exist: %v", spans.created())
	}

	gw.reload(setTelemetry(otelAt("http://127.0.0.1:4318")))
	gw.call("/v1/messages", key, testkit.AnthropicBody())
	gw.waitMetric(t, okClaude, 2)
	if got := spans.created(); len(got) != 1 || got[0] != "http://127.0.0.1:4318" {
		t.Fatalf("exporters %v", got)
	}
	if roots := names(gw.spans(t, spans))["comporta.request"]; roots != 1 {
		t.Fatalf("%d traces after enabling", roots)
	}

	gw.reload(setTelemetry("telemetry:\n  metrics_enabled: false\n  otel_enabled: true\n  otel_endpoint: http://127.0.0.1:4318\n"))
	if gw.telemetry.MetricsEnabled() {
		t.Fatal("metrics still exposed")
	}
	gw.call("/v1/messages", key, testkit.AnthropicBody())
	if roots := waitRoots(t, gw, spans, 2); roots != 2 {
		t.Fatalf("%d traces", roots)
	}
	if got := metric(gw.scrape(t), okClaude); got != 2 {
		t.Fatalf("counted while disabled: %v", got)
	}
	// The same endpoint across reloads keeps the same provider.
	if got := spans.created(); len(got) != 1 {
		t.Fatalf("exporters %v", got)
	}
}

func waitRoots(t *testing.T, gw *gateway, spans *traces, want int) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		roots := names(gw.spans(t, spans))["comporta.request"]
		if roots >= want || time.Now().After(deadline) {
			return roots
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestTelemetryReloadInFlight(t *testing.T) {
	release := make(chan struct{})
	reached := make(chan struct{}, 8)
	options := gatewayOptions{metered: true, edit: appendTelemetry(otelAt("http://127.0.0.1:4001"))}
	spans := observed(&options)
	gw := startConfiguredGateway(t, fakeprovider.Options{
		ByModel: map[string]fakeprovider.Scenario{"fake-gpt": fakeprovider.ScenarioSlowStream},
		Sleep: func(ctx context.Context, _ time.Duration) error {
			select {
			case reached <- struct{}{}:
			default:
			}
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	}, options)
	key := gw.createKey("bot").Key.Reveal()
	var wg sync.WaitGroup
	var status int
	wg.Go(func() {
		response, _ := gw.call("/v1/chat/completions", key, testkit.OpenAIStreamBody())
		status = response.StatusCode
	})
	<-reached
	// The stream in flight keeps the provider of the generation it began with.
	gw.reload(setTelemetry(otelAt("http://127.0.0.1:4002")))
	gw.call("/v1/messages", key, testkit.AnthropicBody())
	close(release)
	wg.Wait()
	if status != http.StatusOK {
		t.Fatalf("stream status %d", status)
	}
	gw.waitMetric(t, `comporta_requests_total{app="bot",model="fake-gpt",operation="generation",outcome="ok"}`, 1)
	if got := spans.created(); len(got) != 2 || got[0] != "http://127.0.0.1:4001" || got[1] != "http://127.0.0.1:4002" {
		t.Fatalf("exporters %v", got)
	}
	if err := gw.telemetry.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	old, current := names(spans.spansOf(0)), names(spans.spansOf(1))
	if old["comporta.request"] != 1 || old["upstream_attempt"] != 1 || old["deliver"] != 1 {
		t.Fatalf("the request in flight lost its trace: %v", old)
	}
	if current["comporta.request"] != 1 {
		t.Fatalf("the new request is not on the new provider: %v", current)
	}
	// The drained provider is gone once its last session ended.
	if p := gw.telemetry.Providers(); p != 1 {
		t.Fatalf("%d providers after draining", p)
	}
}

func TestInvalidOTLPEndpointKeepsSnapshot(t *testing.T) {
	options := gatewayOptions{metered: true, edit: appendTelemetry(otelAt("http://127.0.0.1:4318"))}
	spans := observed(&options)
	gw := startConfiguredGateway(t, fakeprovider.Options{}, options)
	key := gw.createKey("bot").Key.Reveal()
	before := gw.config.Capture().Generation()
	for _, bad := range []string{"http://otel.example:4318", "https://user:secret@otel.example", "http://8.8.8.8:4318"} {
		text := setTelemetry(otelAt(bad))(gw.baseYAML)
		if err := os.WriteFile(gw.path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		if status := gw.config.Reload(context.Background()); status.Result == config.ResultApplied {
			t.Fatalf("%s was applied", bad)
		}
	}
	if gw.config.Capture().Generation() != before {
		t.Fatal("an invalid endpoint replaced the snapshot")
	}
	gw.call("/v1/messages", key, testkit.AnthropicBody())
	if roots := waitRoots(t, gw, spans, 1); roots != 1 {
		t.Fatalf("%d traces on the kept configuration", roots)
	}
	if got := spans.created(); len(got) != 1 || got[0] != "http://127.0.0.1:4318" {
		t.Fatalf("exporters %v", got)
	}
}
