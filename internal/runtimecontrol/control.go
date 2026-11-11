// Package runtimecontrol holds the operator overrides that act on live traffic:
// pauses and the temporary barrier of a user deletion. It knows nothing about
// HTTP, the proxy or the CLI; they reach it through narrow interfaces.
package runtimecontrol

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/storage"
)

var (
	// ErrPaused refuses an admission of a paused app.
	ErrPaused = errors.New("app is paused by an operator")
	// ErrMaintenance refuses an admission of a user being deleted.
	ErrMaintenance = errors.New("user is under maintenance")
	// ErrUnavailable means the control state was never loaded: admission fails
	// closed, whatever the financial fail mode says.
	ErrUnavailable = errors.New("runtime control unavailable")
)

// Subject is one end user of one app, as the proxy resolved it.
type Subject struct {
	App       string
	Hash      string
	Anonymous bool
}

// Control decides admissions against the pause projection and keeps count of the
// tickets in flight, so that a pause acknowledgement and a deletion barrier are
// linearizable with every admission.
type Control struct {
	store storage.PauseStore

	// operations serializes pause changes and maintenance serializes
	// deletions. Both are taken before mu, never while it is held, and a
	// deletion never waits on a pause, so an operator can pause during one.
	operations  sync.Mutex
	maintenance sync.Mutex

	mu       sync.Mutex
	loaded   bool
	state    storage.PauseState
	tickets  map[Subject]int
	barriers []*Barrier
	released chan struct{}
}

// New returns a control that refuses everything until Load succeeds.
func New(store storage.PauseStore) *Control {
	return &Control{store: store, tickets: map[Subject]int{}, released: make(chan struct{})}
}

// Load reads the persisted overrides. It runs before the listeners open.
func (c *Control) Load(ctx context.Context) error {
	state, err := c.store.LoadPause(ctx)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.state, c.loaded = state, true
	c.mu.Unlock()
	return nil
}

// Ticket is one admitted request. It must be released exactly once, after the
// request's completion reached usage accounting.
type Ticket struct {
	control *Control
	subject Subject
	once    sync.Once
}

// Release ends the ticket. Repeated calls do nothing.
func (t *Ticket) Release() {
	if t == nil {
		return
	}
	t.once.Do(func() { t.control.release(t.subject) })
}

// Admit decides one admission and takes its ticket in the same critical
// section, so no admission passes after a pause was acknowledged.
func (c *Control) Admit(subject Subject) (*Ticket, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.loaded {
		return nil, ErrUnavailable
	}
	if c.state.Global || paused(c.state, subject.App) {
		return nil, ErrPaused
	}
	if !subject.Anonymous {
		for _, barrier := range c.barriers {
			if barrier.covers(subject) {
				return nil, ErrMaintenance
			}
		}
	}
	c.tickets[subject]++
	return &Ticket{control: c, subject: subject}, nil
}

func (c *Control) release(subject Subject) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tickets[subject] <= 1 {
		delete(c.tickets, subject)
	} else {
		c.tickets[subject]--
	}
	if len(c.barriers) > 0 {
		close(c.released)
		c.released = make(chan struct{})
	}
}

func paused(state storage.PauseState, app string) bool {
	for _, entry := range state.Apps {
		if entry.App == app && entry.Paused {
			return true
		}
	}
	return false
}

// InFlight counts the tickets not yet released.
func (c *Control) InFlight() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	total := 0
	for _, n := range c.tickets {
		total += n
	}
	return total
}

// State returns a copy of the pause projection.
func (c *Control) State() storage.PauseState {
	c.mu.Lock()
	defer c.mu.Unlock()
	state := c.state
	state.Apps = slices.Clone(c.state.Apps)
	return state
}

// PauseResult is the effective state after an operator action.
type PauseResult struct {
	State   storage.PauseState
	Changed bool
	At      time.Time
}

// Apply persists a pause change and only then publishes it. When the call
// returns without error, every later admission sees the new state; a failed
// commit keeps the previous projection.
func (c *Control) Apply(ctx context.Context, change storage.PauseChange) (PauseResult, error) {
	c.operations.Lock()
	defer c.operations.Unlock()
	state, changed, err := c.store.SavePause(ctx, change)
	if err != nil {
		return PauseResult{}, err
	}
	c.mu.Lock()
	c.state = state
	state.Apps = slices.Clone(state.Apps)
	c.mu.Unlock()
	return PauseResult{State: state, Changed: changed, At: change.At}, nil
}

// HashFunc computes the pseudonym of the user being deleted in one app. It
// closes over the raw identifier, which lives only in memory and only while
// the barrier exists.
type HashFunc func(app string) (string, error)

// Barrier holds back the admissions of one identified user, in every app,
// while that user is deleted. Other users keep being admitted.
type Barrier struct {
	control *Control
	hash    HashFunc
	hashes  map[string]string
	ended   bool
}

// covers runs under the control lock. An app first seen while the barrier is
// up, such as one added by a reload, joins it before its first admission; if
// its pseudonym cannot be computed, the admission is refused.
func (b *Barrier) covers(subject Subject) bool {
	hash, ok := b.hashes[subject.App]
	if !ok {
		computed, err := b.hash(subject.App)
		if err != nil {
			return true
		}
		b.hashes[subject.App], hash = computed, computed
	}
	return hash == subject.Hash
}

// BeginForget installs the barrier of one user. Deletions are serialized: a
// second one waits for the first to end.
func (c *Control) BeginForget(ctx context.Context, hash HashFunc) (*Barrier, error) {
	locked := make(chan struct{})
	go func() {
		c.maintenance.Lock()
		close(locked)
	}()
	select {
	case <-locked:
	case <-ctx.Done():
		go func() {
			<-locked
			c.maintenance.Unlock()
		}()
		return nil, ctx.Err()
	}
	barrier := &Barrier{control: c, hash: hash, hashes: map[string]string{}}
	c.mu.Lock()
	c.barriers = append(c.barriers, barrier)
	c.mu.Unlock()
	return barrier, nil
}

// Include adds apps to the barrier, such as the apps found only in history.
func (b *Barrier) Include(apps []string) error {
	c := b.control
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, app := range apps {
		if _, ok := b.hashes[app]; ok {
			continue
		}
		hash, err := b.hash(app)
		if err != nil {
			return err
		}
		b.hashes[app] = hash
	}
	return nil
}

// Drain waits until no ticket of the user is still open. It never cancels a
// request: when ctx ends first the deletion gives up instead.
func (b *Barrier) Drain(ctx context.Context) error {
	c := b.control
	for {
		c.mu.Lock()
		busy := false
		for subject := range c.tickets {
			if !subject.Anonymous && b.covers(subject) {
				busy = true
				break
			}
		}
		released := c.released
		c.mu.Unlock()
		if !busy {
			return nil
		}
		select {
		case <-released:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Subjects lists the pseudonyms the barrier covers, one per app.
func (b *Barrier) Subjects() []storage.SubjectRef {
	c := b.control
	c.mu.Lock()
	defer c.mu.Unlock()
	subjects := make([]storage.SubjectRef, 0, len(b.hashes))
	for app, hash := range b.hashes {
		subjects = append(subjects, storage.SubjectRef{App: app, Hash: hash})
	}
	slices.SortFunc(subjects, func(a, b storage.SubjectRef) int {
		switch {
		case a.App < b.App:
			return -1
		case a.App > b.App:
			return 1
		}
		return 0
	})
	return subjects
}

// End removes the barrier and forgets the pseudonyms and the identifier.
func (b *Barrier) End() {
	c := b.control
	c.mu.Lock()
	if b.ended {
		c.mu.Unlock()
		return
	}
	b.ended = true
	c.barriers = slices.DeleteFunc(c.barriers, func(other *Barrier) bool { return other == b })
	b.hash, b.hashes = nil, nil
	c.mu.Unlock()
	c.maintenance.Unlock()
}
