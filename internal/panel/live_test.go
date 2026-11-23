package panel

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/events"
	"github.com/JoaoVictorVM/comporta/internal/storage"
)

// startHub runs a hub driven by a manual tick and returns a function that
// ticks once and waits until the tick was handled.
func startHub(t *testing.T, o LiveOptions) (*LiveHub, func()) {
	t.Helper()
	tick := make(chan time.Time)
	o.Tick = tick
	h := NewLiveHub(o)
	handled := make(chan struct{})
	h.ticked = func() { handled <- struct{}{} }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = h.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	step := func() {
		tick <- time.Now()
		<-handled
	}
	return h, step
}

func pending(t *testing.T, h *LiveHub, c *liveClient) (Scope, bool) {
	t.Helper()
	select {
	case <-c.signal:
		return h.take(c), true
	default:
		return 0, false
	}
}

func TestLiveCoalescesToOnePerSecond(t *testing.T) {
	bus := &events.Bus{}
	h, step := startHub(t, LiveOptions{Bus: bus})
	c, err := h.join()
	if err != nil {
		t.Fatal(err)
	}
	if s, ok := pending(t, h, c); !ok || s != allScopes {
		t.Fatalf("a new stream starts with a full refresh, got %v", s)
	}
	step()
	for range 50 {
		h.Mark(ScopeRequests)
		h.Mark(ScopeEvents)
	}
	// Nothing goes out between intervals, however many writes happen.
	if _, ok := pending(t, h, c); ok {
		t.Fatal("an invalidation went out before the interval")
	}
	step()
	s, ok := pending(t, h, c)
	if !ok || s != ScopeRequests|ScopeEvents {
		t.Fatalf("one merged invalidation expected, got %v %v", s, ok)
	}
	if _, ok := pending(t, h, c); ok {
		t.Fatal("more than one invalidation in one interval")
	}
	step()
	if _, ok := pending(t, h, c); ok {
		t.Fatal("an idle interval sent an invalidation")
	}
	// A write notified on the bus reaches the stream at a later interval.
	bus.Notify(events.New(events.ConfigChanged, time.Now()))
	for range 100 {
		step()
		if s, ok := pending(t, h, c); ok {
			if s&ScopeConfiguration == 0 {
				t.Fatalf("config change invalidated %v", s)
			}
			return
		}
	}
	t.Fatal("the bus notification never reached the stream")
}

func TestSlowClientIsBounded(t *testing.T) {
	h, step := startHub(t, LiveOptions{})
	c, _ := h.join()
	h.take(c)
	<-c.signal
	for _, s := range []Scope{ScopeOverview, ScopeRequests, ScopeEvents, ScopeProviders} {
		h.Mark(s)
		step()
	}
	if len(c.signal) != 1 {
		t.Fatalf("a slow client holds %d signals", len(c.signal))
	}
	s, _ := pending(t, h, c)
	if s != ScopeOverview|ScopeRequests|ScopeEvents|ScopeProviders {
		t.Fatalf("the pending set lost scopes: %v", s)
	}
	for range MaxStreams - 1 {
		if _, err := h.join(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.join(); err != ErrStreamsFull {
		t.Fatalf("stream %d accepted", MaxStreams+1)
	}
}

func TestBusLossForcesResync(t *testing.T) {
	bus := &events.Bus{}
	h, step := startHub(t, LiveOptions{Bus: bus})
	c, _ := h.join()
	h.take(c)
	<-c.signal
	step()
	// Holding the hub keeps it from draining the bus until the subscription
	// overflows and records the loss.
	h.mu.Lock()
	var wg sync.WaitGroup
	wg.Go(func() {
		for range 600 {
			bus.Notify(events.New(events.Paused, time.Now()))
		}
	})
	wg.Wait()
	h.mu.Unlock()
	step()
	step()
	var got Scope
	for {
		s, ok := pending(t, h, c)
		if !ok {
			break
		}
		got |= s
	}
	if got != allScopes {
		t.Fatalf("a lost notification must refresh everything, got %v", got)
	}
}

func TestSharedWatermarkFindsRemoteWrite(t *testing.T) {
	var mu sync.Mutex
	mark := storage.PanelWatermark{RequestsLatest: 1}
	reads := 0
	h, step := startHub(t, LiveOptions{Watermark: func(context.Context) (storage.PanelWatermark, error) {
		mu.Lock()
		defer mu.Unlock()
		reads++
		return mark, nil
	}})
	clients := make([]*liveClient, 5)
	for i := range clients {
		clients[i], _ = h.join()
		h.take(clients[i])
		<-clients[i].signal
	}
	step()
	for _, c := range clients {
		if _, ok := pending(t, h, c); ok {
			t.Fatal("an unchanged watermark invalidated")
		}
	}
	mu.Lock()
	mark.RequestsRecent = 3
	mu.Unlock()
	step()
	for _, c := range clients {
		if s, ok := pending(t, h, c); !ok || s&ScopeRequests == 0 {
			t.Fatalf("a write by another replica was not seen: %v", s)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	// One read per interval, whatever the number of browsers.
	if reads != 2 {
		t.Fatalf("watermark read %d times in two steps", reads)
	}
}
