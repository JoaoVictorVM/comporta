package runtimecontrol

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// pseudonym stands in for the HMAC: deterministic per app and identifier.
func pseudonym(id string) HashFunc {
	return func(app string) (string, error) {
		return strings.Repeat("0", 32-len(app)) + app + id, nil
	}
}

func hashOf(app, id string) string {
	h, _ := pseudonym(id)(app)
	return h
}

func TestForgetBarrierBlocksOnlyThatUser(t *testing.T) {
	control := loaded(t, openStore(t))
	barrier, err := control.BeginForget(context.Background(), pseudonym(""))
	if err != nil {
		t.Fatal(err)
	}
	if err := barrier.Include([]string{"bot"}); err != nil {
		t.Fatal(err)
	}
	target := Subject{App: "bot", Hash: hashOf("bot", "")}
	if _, err := control.Admit(target); !errors.Is(err, ErrMaintenance) {
		t.Fatalf("the user being deleted: %v", err)
	}
	if _, err := control.Admit(Subject{App: "bot", Hash: strings.Repeat("9", 32)}); err != nil {
		t.Fatalf("another user: %v", err)
	}
	anonymous := target
	anonymous.Anonymous = true
	if _, err := control.Admit(anonymous); err != nil {
		t.Fatalf("the anonymous bucket is never the identified user: %v", err)
	}
	barrier.End()
	barrier.End()
	if _, err := control.Admit(target); err != nil {
		t.Fatalf("after the deletion the user is admitted again: %v", err)
	}
}

func TestBarrierDrainWaitsForTickets(t *testing.T) {
	control := loaded(t, openStore(t))
	target := Subject{App: "bot", Hash: hashOf("bot", "")}
	held, err := control.Admit(target)
	if err != nil {
		t.Fatal(err)
	}
	unrelated, _ := control.Admit(Subject{App: "bot", Hash: strings.Repeat("9", 32)})
	barrier, err := control.BeginForget(context.Background(), pseudonym(""))
	if err != nil {
		t.Fatal(err)
	}
	defer barrier.End()
	drained := make(chan error, 1)
	go func() { drained <- barrier.Drain(context.Background()) }()
	unrelated.Release()
	select {
	case err := <-drained:
		t.Fatalf("the drain ended while the user's request was open: %v", err)
	default:
	}
	held.Release()
	if err := <-drained; err != nil {
		t.Fatal(err)
	}
}

func TestBarrierDrainTimesOut(t *testing.T) {
	control := loaded(t, openStore(t))
	held, _ := control.Admit(Subject{App: "bot", Hash: hashOf("bot", "")})
	defer held.Release()
	barrier, err := control.BeginForget(context.Background(), pseudonym(""))
	if err != nil {
		t.Fatal(err)
	}
	defer barrier.End()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := barrier.Drain(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a request that never ends: %v", err)
	}
}

func TestBarrierCoversAppAddedDuringOperation(t *testing.T) {
	control := loaded(t, openStore(t))
	barrier, err := control.BeginForget(context.Background(), pseudonym(""))
	if err != nil {
		t.Fatal(err)
	}
	defer barrier.End()
	if err := barrier.Include([]string{"bot"}); err != nil {
		t.Fatal(err)
	}
	fresh := Subject{App: "added-by-reload", Hash: hashOf("added-by-reload", "")}
	if _, err := control.Admit(fresh); !errors.Is(err, ErrMaintenance) {
		t.Fatalf("an app unknown when the barrier went up: %v", err)
	}
	found := false
	for _, subject := range barrier.Subjects() {
		found = found || subject.App == "added-by-reload"
	}
	if !found {
		t.Fatal("the new app did not join the deletion")
	}
}

func TestBarrierFailsClosedWithoutPseudonym(t *testing.T) {
	control := loaded(t, openStore(t))
	barrier, err := control.BeginForget(context.Background(), func(string) (string, error) { return "", errors.New("no key") })
	if err != nil {
		t.Fatal(err)
	}
	defer barrier.End()
	if _, err := control.Admit(Subject{App: "bot", Hash: strings.Repeat("9", 32)}); !errors.Is(err, ErrMaintenance) {
		t.Fatalf("an app whose pseudonym cannot be computed must refuse: %v", err)
	}
}

func TestForgetsAreSerialized(t *testing.T) {
	control := loaded(t, openStore(t))
	first, err := control.BeginForget(context.Background(), pseudonym(""))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := control.BeginForget(ctx, pseudonym("x")); err == nil {
		t.Fatal("a second deletion started during the first")
	}
	first.End()
	second, err := control.BeginForget(context.Background(), pseudonym("x"))
	if err != nil {
		t.Fatalf("after the first ended: %v", err)
	}
	second.End()
}
