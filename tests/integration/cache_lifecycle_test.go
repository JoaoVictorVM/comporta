package integration

import (
	"context"
	"os"
	"testing"

	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/fakeprovider"
)

func TestCacheInvalidatesOnAppliedReload(t *testing.T) {
	gw := startCacheGateway(t, cacheOn, fakeprovider.Options{}, gatewayOptions{})
	key := gw.createKey("bot").Key.Reveal()
	body := zeroTemp(gptBody)
	gw.call("/v1/chat/completions", key, body)
	if response, _ := gw.call("/v1/chat/completions", key, body); response.Header.Get("X-Comporta-Cache") != "hit" {
		t.Fatal("expected a hit before the reload")
	}
	// Any applied change is a new generation, even one unrelated to content.
	gw.reload(func(text string) string { return text + "\n# comment only\nstorage:\n  retention_days: 29\n" })
	if response, _ := gw.call("/v1/chat/completions", key, body); response.Header.Get("X-Comporta-Cache") != "" {
		t.Fatal("an applied reload invalidates every entry")
	}
	if gw.providerCalls() != 2 {
		t.Fatal("provider calls")
	}
}

func TestRejectedReloadKeepsCache(t *testing.T) {
	gw := startCacheGateway(t, cacheOn, fakeprovider.Options{}, gatewayOptions{})
	key := gw.createKey("bot").Key.Reveal()
	body := zeroTemp(gptBody)
	gw.call("/v1/chat/completions", key, body)
	if err := os.WriteFile(gw.path, []byte(gw.baseYAML+"\nunknown_root: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if status := gw.config.Reload(context.Background()); status.Result != config.ResultRejected {
		t.Fatalf("reload %+v", status)
	}
	if err := os.WriteFile(gw.path, []byte(gw.baseYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	if status := gw.config.Reload(context.Background()); status.Result != config.ResultUnchanged {
		t.Fatalf("reload %+v", status)
	}
	if response, _ := gw.call("/v1/chat/completions", key, body); response.Header.Get("X-Comporta-Cache") != "hit" {
		t.Fatal("a rejected or unchanged reload keeps the cache")
	}
}
