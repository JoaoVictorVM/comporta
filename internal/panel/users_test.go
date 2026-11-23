package panel

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestSearchUsesAppScopedHashes(t *testing.T) {
	env := newEnv(t)
	at := time.Date(2026, 11, 18, 15, 0, 0, 0, time.UTC)
	env.commit(t, env.record(t, 1, "bot", "customer-42", at, 1_500), env.record(t, 2, "web", "customer-42", at, 700), env.record(t, 3, "bot", "someone-else", at, 9_000))
	result := env.service.SearchUser(context.Background(), UserQuery{ID: "customer-42"})
	if result.Invalid || result.Spend != Available || len(result.Groups) != 2 {
		t.Fatalf("search: %+v", result)
	}
	for _, group := range result.Groups {
		hash, _ := env.identity.LookupHash(group.App, "customer-42")
		if group.Hash != ShortHash(hash.Hex()) {
			t.Fatalf("%s: hash %s is not the app-scoped pseudonym", group.App, group.Hash)
		}
		if len(group.History) != 1 {
			t.Fatalf("%s history: %+v", group.App, group.History)
		}
	}
	if result.Groups[0].App != "bot" || result.Groups[0].History[0].Cost != 1_500 || result.Groups[1].History[0].Cost != 700 {
		t.Fatalf("groups: %+v", result.Groups)
	}
	if result.Groups[0].Hash == result.Groups[1].Hash {
		t.Fatal("the same id has the same pseudonym in two apps")
	}
	only := env.service.SearchUser(context.Background(), UserQuery{ID: "customer-42", App: "web"})
	if len(only.Groups) != 1 || only.Groups[0].App != "web" {
		t.Fatalf("app filter: %+v", only)
	}
	// The user limits are read for the user's own pseudonym.
	last := env.limits.requests[len(env.limits.requests)-1]
	hash, _ := env.identity.LookupHash("web", "customer-42")
	if last == nil || last.Hash != hash.Hex() || last.App != "web" {
		t.Fatalf("limit state read for %+v", last)
	}
}

func TestSearchNeverReturnsRawID(t *testing.T) {
	env := newEnv(t)
	env.commit(t, env.record(t, 1, "bot", "raw-identifier-77", time.Date(2026, 11, 18, 15, 0, 0, 0, time.UTC), 1_500))
	result := env.service.SearchUser(context.Background(), UserQuery{ID: "raw-identifier-77"})
	data, _ := json.Marshal(result)
	if strings.Contains(string(data), "raw-identifier-77") {
		t.Fatal("the result carries the identifier that was typed")
	}
	for _, id := range []string{"", "<script>alert(1)</script>", "someone@example.com", "529.982.247-25", strings.Repeat("a", 300)} {
		if r := env.service.SearchUser(context.Background(), UserQuery{ID: id}); !r.Invalid || len(r.Groups) != 0 {
			t.Fatalf("%q accepted: %+v", id, r)
		}
	}
}

func TestAnonymousIsExplicit(t *testing.T) {
	env := newEnv(t)
	at := time.Date(2026, 11, 18, 15, 0, 0, 0, time.UTC)
	anon := env.record(t, 1, "bot", "x", at, 300)
	anon.UserHash, anon.Anonymous = env.identity.AnonymousHash("bot").Hex(), true
	named := env.record(t, 2, "bot", "anonymous", at, 900)
	env.commit(t, anon, named)
	byName := env.service.SearchUser(context.Background(), UserQuery{ID: "anonymous", App: "bot"})
	if len(byName.Groups) != 1 || byName.Groups[0].Anonymous || byName.Groups[0].Today != 900 {
		t.Fatalf("a user called anonymous merged with the bucket: %+v", byName)
	}
	bucket := env.service.SearchUser(context.Background(), UserQuery{App: "bot", Anonymous: true})
	if len(bucket.Groups) != 1 || !bucket.Groups[0].Anonymous || bucket.Groups[0].Today != 300 {
		t.Fatalf("anonymous bucket: %+v", bucket)
	}
	if r := env.service.SearchUser(context.Background(), UserQuery{Anonymous: true}); !r.Invalid {
		t.Fatal("the anonymous bucket needs its app")
	}
}
