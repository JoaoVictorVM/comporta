package integration

import (
	"bytes"
	"context"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/fakeprovider"
	"github.com/JoaoVictorVM/comporta/internal/testkit"
)

func TestCacheInvalidatesOnAppliedReload(t *testing.T) {
	gw := startCacheGateway(t, cacheOn, fakeprovider.Options{}, gatewayOptions{})
	key := gw.createKey("bot").Key.Reveal()
	body := zeroTemp(gptBody)
	gw.call("/v1/chat/completions", key, body)
	gw.waitCached(t, 1)
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
	gw.waitCached(t, 1)
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

// callCache sends a chat completion through serve and reports whether it was
// answered from the cache.
func (s *served) callCache(app, user, body string) (int, bool) {
	s.t.Helper()
	request, err := http.NewRequest(http.MethodPost, s.proxyURL("/v1/chat/completions"), strings.NewReader(body))
	if err != nil {
		s.t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+s.keys[app])
	if user != "" {
		request.Header.Set("X-Comporta-User", user)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		s.t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.ReadAll(response.Body)
	return response.StatusCode, response.Header.Get("X-Comporta-Cache") == "hit"
}

func servedCache(settings string) func(string) string {
	return withBot(cacheOn + settings)
}

// dataDirHolds reports whether any file under dir contains needle.
func dataDirHolds(t *testing.T, dir, needle string) []string {
	t.Helper()
	var found []string
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		if strings.Contains(strings.ToLower(entry.Name()), "cache") {
			found = append(found, path)
		}
		data, err := os.ReadFile(path)
		if err == nil && bytes.Contains(data, []byte(needle)) {
			found = append(found, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

func TestCacheEmptyAfterRestart(t *testing.T) {
	s := startServedWith(t, fakeprovider.Options{}, servedCache(""))
	body := zeroTemp(gptBody)
	if _, hit := s.callCache("bot", "", body); hit {
		t.Fatal("the first request is a miss")
	}
	if _, hit := s.callCache("bot", "", body); !hit {
		t.Fatal("expected a hit before the restart")
	}
	// The answer id only exists in the provider's payload, so finding it on
	// disk would mean the cached content was written somewhere.
	if files := dataDirHolds(t, s.dataDir, "chatcmpl_fake_1"); len(files) != 0 {
		t.Fatalf("cache content or files on disk: %v", files)
	}
	s.restart()
	if status, hit := s.callCache("bot", "", body); status != http.StatusOK || hit {
		t.Fatalf("after a restart the cache is empty: %d hit=%v", status, hit)
	}
	if files := dataDirHolds(t, s.dataDir, "chatcmpl_fake_1"); len(files) != 0 {
		t.Fatalf("cache content or files on disk: %v", files)
	}
}

func TestCacheForgetBarrier(t *testing.T) {
	s := startServedWith(t, fakeprovider.Options{}, servedCache("      shared_across_users: true\n"))
	body := zeroTemp(gptBody)
	forgotten := testkit.UserMarker()
	s.callCache("bot", forgotten, body)
	if _, hit := s.callCache("bot", "another-user", body); !hit {
		t.Fatal("a shared entry serves another user")
	}
	s.waitStored("bot", s.hash("bot", forgotten), false, 1)
	if status, _, text := s.forget(forgotten); status != http.StatusOK {
		t.Fatalf("forget %d %s", status, text)
	}
	if _, hit := s.callCache("bot", "another-user", body); hit {
		t.Fatal("after users forget the app's entries, shared ones included, are gone")
	}
}

func TestCacheForgetRejectsConcurrentSharedFill(t *testing.T) {
	held := newHeldStream()
	s := startServedWith(t, held.options(), servedCache("      shared_across_users: true\n"))
	body := zeroTemp(gptStream)
	done := make(chan bool, 1)
	go func() {
		_, hit := s.callCache("bot", "filler", body)
		done <- hit
	}()
	<-held.received
	// The forgotten user has nothing in flight, so the barrier does not wait for
	// the filler; its capture began before the purge and must not land.
	if status, _, text := s.forget(testkit.UserMarker()); status != http.StatusOK {
		t.Fatalf("forget %d %s", status, text)
	}
	close(held.release)
	if hit := <-done; hit {
		t.Fatal("the filler was a miss")
	}
	if _, hit := s.callCache("bot", "filler", body); hit {
		t.Fatal("a capture that overlapped the deletion repopulated the app")
	}
	if _, hit := s.callCache("bot", "filler", body); !hit {
		t.Fatal("captures that start after the deletion are stored again")
	}
}

func TestPauseBlocksCachedResponse(t *testing.T) {
	s := startServedWith(t, fakeprovider.Options{}, servedCache(""))
	body := zeroTemp(gptBody)
	s.callCache("bot", "", body)
	if status, body := s.admin(http.MethodPost, "/admin/v1/pause", `{"app":"bot"}`); status != http.StatusOK {
		t.Fatalf("pause %d %s", status, body)
	}
	if status, hit := s.callCache("bot", "", body); status != http.StatusServiceUnavailable || hit {
		t.Fatalf("a paused app never answers from cache: %d hit=%v", status, hit)
	}
	s.admin(http.MethodPost, "/admin/v1/resume", `{"app":"bot"}`)
	if status, hit := s.callCache("bot", "", body); status != http.StatusOK || !hit {
		t.Fatalf("after resuming the entry is still valid: %d hit=%v", status, hit)
	}
}

// waitCached waits until the capture of a response is stored: it is published
// after the answer reached the client, so a follow-up call may otherwise run
// before the entry exists.
func (g *gateway) waitCached(t testing.TB, entries int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for g.cache.Stats().Entries < entries {
		if time.Now().After(deadline) {
			t.Fatal("the response was never cached")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
