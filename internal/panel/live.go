package panel

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/events"
	"github.com/JoaoVictorVM/comporta/internal/storage"
)

// Scope is one live region the stream can invalidate. Only these names ever
// reach the stream.
type Scope uint8

const (
	ScopeOverview Scope = 1 << iota
	ScopeApps
	ScopeRequests
	ScopeEvents
	ScopeProviders
	ScopeConfiguration
)

const allScopes = ScopeOverview | ScopeApps | ScopeRequests | ScopeEvents | ScopeProviders | ScopeConfiguration

var scopeNames = []struct {
	scope Scope
	name  string
}{
	{ScopeOverview, "overview"}, {ScopeApps, "apps"}, {ScopeRequests, "requests"},
	{ScopeEvents, "events"}, {ScopeProviders, "providers"}, {ScopeConfiguration, "configuration"},
}

// String lists the scopes as the stream sends them.
func (s Scope) String() string {
	var names []string
	for _, n := range scopeNames {
		if s&n.scope != 0 {
			names = append(names, n.name)
		}
	}
	return strings.Join(names, ",")
}

// Live limits.
const (
	MaxStreams        = 128
	LiveInterval      = time.Second
	HeartbeatInterval = 15 * time.Second
	streamWriteLimit  = 10 * time.Second
)

// ErrStreamsFull means the process already serves MaxStreams streams.
var ErrStreamsFull = errors.New("live stream capacity reached")

// LiveOptions configures the hub. Watermark is set in deployments where other
// replicas write: it is read once per interval, whatever the number of
// browsers.
type LiveOptions struct {
	Bus       *events.Bus
	Config    ConfigSource
	Watermark func(context.Context) (storage.PanelWatermark, error)
	Logger    *slog.Logger
	// Tick replaces the interval ticker in tests.
	Tick <-chan time.Time
	// Now and After replace the clock of the streams in tests.
	Now   func() time.Time
	After func(time.Duration) <-chan time.Time
}

type liveClient struct {
	pending Scope
	signal  chan struct{}
}

// LiveHub turns writes into invalidations. It never carries a record, a name
// or a value: only which scopes are stale. Invalidations are merged and sent
// at most once per interval; a slow client keeps one pending set and never
// accumulates messages.
type LiveHub struct {
	o       LiveOptions
	mu      sync.Mutex
	clients map[*liveClient]struct{}
	dirty   Scope
	closed  bool
	done    chan struct{}
	// ticked, set only by tests, runs after each interval was handled.
	ticked func()
}

// NewLiveHub returns a hub. Run must be called for invalidations to flow.
func NewLiveHub(o LiveOptions) *LiveHub {
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.After == nil {
		o.After = time.After
	}
	return &LiveHub{o: o, clients: map[*liveClient]struct{}{}, done: make(chan struct{})}
}

// scopesOf maps an event type to the regions it makes stale.
func scopesOf(kind events.Type) Scope {
	switch kind {
	case events.RequestCompleted:
		return ScopeOverview | ScopeApps | ScopeRequests | ScopeProviders
	case events.Blocked, events.Cached, events.Fallback, events.Downgraded, events.Warned, events.LoopCut:
		return ScopeOverview | ScopeApps | ScopeRequests | ScopeEvents | ScopeProviders
	case events.ConfigChanged, events.ConfigReloadFailed:
		return ScopeConfiguration | ScopeOverview | ScopeApps | ScopeEvents | ScopeProviders
	case events.Paused, events.LimitStateUnavailable, events.LimitStateRecovered:
		return ScopeOverview | ScopeApps | ScopeEvents
	}
	return ScopeEvents
}

// Mark makes scopes stale; they go out at the next interval.
func (h *LiveHub) Mark(s Scope) {
	h.mu.Lock()
	h.dirty |= s
	h.mu.Unlock()
}

// Run follows the bus, the configuration and, when set, the shared watermark
// until ctx ends.
func (h *LiveHub) Run(ctx context.Context) error {
	defer h.Shutdown()
	var sub *events.Subscription
	var stream <-chan events.Event
	if h.o.Bus != nil {
		sub = h.o.Bus.Subscribe(ctx)
		stream = sub.Events
	}
	tick := h.o.Tick
	if tick == nil {
		ticker := time.NewTicker(LiveInterval)
		defer ticker.Stop()
		tick = ticker.C
	}
	var lastMark storage.PanelWatermark
	watermarkKnown := false
	var lastGeneration int64
	var lastAttempt time.Time
	if h.o.Config != nil {
		status := h.o.Config.Status()
		lastGeneration, lastAttempt = status.ActiveGeneration, status.LastAttemptAt
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case e, ok := <-stream:
			if !ok {
				stream = nil
				continue
			}
			h.Mark(scopesOf(e.Type))
		case <-tick:
			// A lost notification may have been any write: everything is stale.
			if sub != nil && sub.Resync.Swap(false) {
				h.Mark(allScopes)
			}
			if h.o.Config != nil {
				status := h.o.Config.Status()
				if status.ActiveGeneration != lastGeneration || !status.LastAttemptAt.Equal(lastAttempt) {
					lastGeneration, lastAttempt = status.ActiveGeneration, status.LastAttemptAt
					h.Mark(ScopeConfiguration | ScopeOverview | ScopeApps | ScopeProviders)
				}
			}
			if h.o.Watermark != nil {
				read, cancel := context.WithTimeout(ctx, storage.PanelQueryTimeout)
				mark, err := h.o.Watermark(read)
				cancel()
				switch {
				case err != nil:
					h.o.Logger.Warn("panel watermark unavailable", "class", "unavailable")
				case watermarkKnown && mark != lastMark:
					h.Mark(ScopeOverview | ScopeApps | ScopeRequests | ScopeEvents | ScopeProviders)
					lastMark = mark
				default:
					lastMark, watermarkKnown = mark, true
				}
			}
			h.flush()
			if h.ticked != nil {
				h.ticked()
			}
		}
	}
}

// flush hands the stale scopes to every client. A client that has not taken
// its last set yet simply has the new scopes merged into it.
func (h *LiveHub) flush() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.dirty == 0 {
		return
	}
	for c := range h.clients {
		c.pending |= h.dirty
		select {
		case c.signal <- struct{}{}:
		default:
		}
	}
	h.dirty = 0
}

func (h *LiveHub) join() (*liveClient, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, ErrStreamsFull
	}
	if len(h.clients) >= MaxStreams {
		return nil, ErrStreamsFull
	}
	// A new connection, or a reconnection, starts with everything stale.
	c := &liveClient{pending: allScopes, signal: make(chan struct{}, 1)}
	c.signal <- struct{}{}
	h.clients[c] = struct{}{}
	return c, nil
}

func (h *LiveHub) leave(c *liveClient) {
	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
}

func (h *LiveHub) take(c *liveClient) Scope {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := c.pending
	c.pending = 0
	return s
}

// Streams counts the open streams.
func (h *LiveHub) Streams() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients)
}

// Shutdown ends every stream, so a drain never waits on an open browser.
func (h *LiveHub) Shutdown() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.closed {
		h.closed = true
		close(h.done)
	}
}

// serve writes one stream until the request ends, the session expires or the
// hub shuts down.
func (h *LiveHub) serve(w http.ResponseWriter, r *http.Request, expires time.Time) error {
	c, err := h.join()
	if err != nil {
		return err
	}
	defer h.leave(c)
	commonHeaders(w)
	header := w.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-store")
	header.Set("X-Accel-Buffering", "no")
	controller := http.NewResponseController(w)
	write := func(text string) bool {
		_ = controller.SetWriteDeadline(time.Now().Add(streamWriteLimit))
		if _, err := w.Write([]byte(text)); err != nil {
			return false
		}
		return controller.Flush() == nil
	}
	w.WriteHeader(http.StatusOK)
	if !write("retry: 2000\n\n") {
		return nil
	}
	expiry := h.o.After(expires.Sub(h.o.Now()))
	for {
		select {
		case <-r.Context().Done():
			return nil
		case <-h.done:
			return nil
		case <-expiry:
			write("event: session-expired\ndata: expired\n\n")
			return nil
		case <-c.signal:
			if s := h.take(c); s != 0 && !write("event: refresh\ndata: "+s.String()+"\n\n") {
				return nil
			}
		case <-h.o.After(HeartbeatInterval):
			if !write(": heartbeat\n\n") {
				return nil
			}
		}
	}
}
