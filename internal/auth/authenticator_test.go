package auth

import (
	"context"
	"crypto/subtle"
	"errors"
	"testing"

	"github.com/JoaoVictorVM/comporta/internal/keys"
)

func TestAuthenticationReturnsConfiguredAppAndKeyID(t *testing.T) {
	f := newFixture(t)
	f.create(t, "other")
	created := f.create(t, "bot")
	manager := f.keyManager(t)
	authenticator := NewAuthenticator(manager, nil)

	identity, err := authenticator.Authenticate(created.Key, manager.Capture(), f.config.Capture())
	if err != nil {
		t.Fatal(err)
	}
	if identity.AppName() != "bot" || identity.KeyID() != created.Metadata.ID {
		t.Fatalf("identity %+v", identity)
	}
}

func TestUnknownKeyRejected(t *testing.T) {
	f := newFixture(t)
	f.create(t, "bot")
	manager := f.keyManager(t)
	unknown, err := keys.ParseKey("cmp_" + "0000000000000000000000000000000000000000000")
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewAuthenticator(manager, nil).Authenticate(unknown, manager.Capture(), f.config.Capture())
	if !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("err = %v", err)
	}
}

func TestRevokedKeyRejected(t *testing.T) {
	f := newFixture(t)
	created := f.create(t, "bot")
	if _, err := f.store.Revoke(context.Background(), created.Metadata.ID); err != nil {
		t.Fatal(err)
	}
	manager := f.keyManager(t)
	_, err := NewAuthenticator(manager, nil).Authenticate(created.Key, manager.Capture(), f.config.Capture())
	if !errors.Is(err, ErrRevokedKey) {
		t.Fatalf("err = %v", err)
	}
	if err.Error() != "this Comporta API key was revoked" {
		t.Fatalf("message %q", err)
	}
}

func TestRemovedAppRejected(t *testing.T) {
	f := newFixture(t)
	created := f.create(t, "other")
	manager := f.keyManager(t)
	authenticator := NewAuthenticator(manager, nil)
	f.removeApp(t, "other")
	if _, err := authenticator.Authenticate(created.Key, manager.Capture(), f.config.Capture()); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("err = %v", err)
	}
}

func TestConfigReloadRemovesAppFromAuthentication(t *testing.T) {
	f := newFixture(t)
	created := f.create(t, "other")
	manager := f.keyManager(t)
	authenticator := NewAuthenticator(manager, nil)
	before := f.config.Capture()

	f.removeApp(t, "other")
	if _, err := authenticator.Authenticate(created.Key, manager.Capture(), f.config.Capture()); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("removed app: %v", err)
	}
	if _, err := authenticator.Authenticate(created.Key, manager.Capture(), before); err != nil {
		t.Fatalf("an operation holding the earlier snapshot keeps its rules: %v", err)
	}
	entries, err := f.store.List()
	if err != nil || len(entries) != 1 || !entries[0].Active() {
		t.Fatalf("the stored credential must not change: %+v %v", entries, err)
	}

	f.restoreConfig(t)
	identity, err := authenticator.Authenticate(created.Key, manager.Capture(), f.config.Capture())
	if err != nil || identity.AppName() != "other" {
		t.Fatalf("restoring the app restores the key: %+v %v", identity, err)
	}
}

func TestUnavailableGenerationNeverAuthorizes(t *testing.T) {
	f := newFixture(t)
	created := f.create(t, "bot")
	manager := f.keyManager(t)
	var unavailable keys.Generation
	_, err := NewAuthenticator(manager, nil).Authenticate(created.Key, &unavailable, f.config.Capture())
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v", err)
	}
}

func TestComparisonVisitsEveryDigest(t *testing.T) {
	f := newFixture(t)
	first := f.create(t, "bot")
	f.create(t, "bot")
	f.create(t, "other")
	revoked := f.create(t, "other")
	if _, err := f.store.Revoke(context.Background(), revoked.Metadata.ID); err != nil {
		t.Fatal(err)
	}
	manager := f.keyManager(t)
	authenticator := NewAuthenticator(manager, nil)
	calls := 0
	authenticator.compare = func(a, b []byte) int {
		calls++
		return subtle.ConstantTimeCompare(a, b)
	}
	if _, err := authenticator.Authenticate(first.Key, manager.Capture(), f.config.Capture()); err != nil {
		t.Fatal(err)
	}
	if calls != 4 {
		t.Fatalf("visited %d digests of 4: a match must not end the walk", calls)
	}
	if NewAuthenticator(manager, nil).compare == nil {
		t.Fatal("the default comparison must be set")
	}
}

func TestSuccessRecordsUsageWithoutTouchingDisk(t *testing.T) {
	f := newFixture(t)
	created := f.create(t, "bot")
	manager := f.keyManager(t)
	clock := newFakeClock()
	if _, err := NewAuthenticator(manager, clock).Authenticate(created.Key, manager.Capture(), f.config.Capture()); err != nil {
		t.Fatal(err)
	}
	entries, err := f.store.List()
	if err != nil {
		t.Fatal(err)
	}
	if !entries[0].LastUsedAt.IsZero() {
		t.Fatal("usage is saved in groups, not per request")
	}
}
