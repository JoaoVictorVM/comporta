//go:build distributed

package postgres

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/admin"
	"github.com/JoaoVictorVM/comporta/internal/auth"
	"github.com/JoaoVictorVM/comporta/internal/keys"
)

func TestConcurrentThirdKeyRejected(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	results := make([]error, 6)
	for i := range results {
		wg.Go(func() { _, results[i] = s.Keys().Create(ctx, "bot") })
	}
	wg.Wait()
	created, limited := 0, 0
	for _, err := range results {
		var limit *keys.ActiveLimitError
		switch {
		case err == nil:
			created++
		case errors.As(err, &limit):
			limited++
		default:
			t.Fatal(err)
		}
	}
	if created != keys.MaxActiveKeysPerApp || limited != len(results)-keys.MaxActiveKeysPerApp {
		t.Fatalf("expected exactly two active keys, created %d", created)
	}
}

func TestSharedAppKeyRotation(t *testing.T) {
	a, dsn := openTest(t)
	b, err := Open(context.Background(), Options{DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	ctx := context.Background()
	created, err := a.Keys().Create(ctx, "bot")
	if err != nil {
		t.Fatal(err)
	}
	source, err := keys.NewSharedSource(ctx, b.Keys(), nil)
	if err != nil {
		t.Fatal(err)
	}
	found := func() (bool, bool) {
		for e := range source.Capture().Entries() {
			if e.Digest == created.Key.Digest() {
				return true, e.Revoked
			}
		}
		return false, false
	}
	if ok, _ := found(); !ok {
		t.Fatal("a key created on one replica must authenticate on another")
	}
	if changed, err := a.Keys().Revoke(ctx, created.Metadata.ID); err != nil || !changed {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go source.Run(runCtx)
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, revoked := found(); revoked {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("revocation did not reach the other replica within 2 seconds")
		}
		time.Sleep(50 * time.Millisecond)
	}
	list, err := b.Keys().List()
	if err != nil || len(list) != 1 || list[0].Active() {
		t.Fatalf("list: %+v %v", list, err)
	}
}

func TestAdminSessionAcrossReplicas(t *testing.T) {
	a, dsn := openTest(t)
	b, err := Open(context.Background(), Options{DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	ctx := context.Background()
	if _, err := admin.OpenSharedCredential(ctx, a); !errors.Is(err, admin.ErrNoSharedCredential) {
		t.Fatalf("an uninitialized cluster has no credential: %v", err)
	}
	token, err := admin.ResetSharedToken(ctx, a, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ResetSharedToken(ctx, a, true); err == nil {
		t.Fatal("the initial token is created once")
	}
	credA, err := admin.OpenSharedCredential(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	credB, err := admin.OpenSharedCredential(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	if !credA.Verify(token) || !credB.Verify(token) || credB.Verify("0000000000000000000000000000000000000000000000000000000000000000") {
		t.Fatal("token must verify on both replicas only")
	}
	id, _, err := admin.NewSharedSessions(a, credA).Create()
	if err != nil {
		t.Fatal(err)
	}
	sessionsB := admin.NewSharedSessions(b, credB)
	if !sessionsB.Valid(id) {
		t.Fatal("a session from one replica must be valid on another without sticky sessions")
	}
	rotated, err := admin.ResetSharedToken(ctx, b, false)
	if err != nil {
		t.Fatal(err)
	}
	if sessionsB.Valid(id) {
		t.Fatal("rotation must end every session")
	}
	time.Sleep(1100 * time.Millisecond)
	if credA.Verify(token) || !credA.Verify(rotated) {
		t.Fatal("rotation must reach the other replica within its cache bound")
	}
}

func TestSharedAuthThrottle(t *testing.T) {
	a, dsn := openTest(t)
	b, err := Open(context.Background(), Options{DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	key := make([]byte, 32)
	options := auth.GuardOptions{MaxFailures: 4, Window: time.Minute, Cooldown: time.Minute}
	guardA := auth.NewSharedGuard(a.AuthWindows(), "admin", key, nil, options)
	guardB := auth.NewSharedGuard(b.AuthWindows(), "admin", key, nil, options)
	addr := netip.MustParseAddr("203.0.113.9")
	for i := range 4 {
		guard := guardA
		if i%2 == 1 {
			guard = guardB
		}
		if d := guard.Failure(addr); !d.Allowed {
			t.Fatalf("failure %d was refused early", i)
		}
	}
	if d := guardA.Admit(addr); d.Allowed {
		t.Fatal("failures split across replicas must add up to the block")
	}
	if d := guardB.Admit(addr); d.Allowed || d.RetryAfter <= 0 {
		t.Fatal("the block must hold on every replica")
	}
	if d := guardB.Admit(netip.MustParseAddr("203.0.113.10")); !d.Allowed {
		t.Fatal("another origin is not blocked")
	}
	if count(t, a, `SELECT count(*) FROM auth_windows WHERE origin_digest::text LIKE '%203%'`) != 0 {
		t.Fatal("raw addresses must never be stored")
	}
	a.Close()
	if d := guardA.Admit(netip.MustParseAddr("198.51.100.1")); d.Allowed {
		t.Fatal("an unavailable guard store must refuse, not let attempts through uncounted")
	}
}
