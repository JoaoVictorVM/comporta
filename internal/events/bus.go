package events

import (
	"context"
	"sync"
	"sync/atomic"
)

type Subscription struct {
	Events <-chan Event
	Resync atomic.Bool
	Lost   atomic.Uint64
}
type subscriber struct {
	sub   *Subscription
	ch    chan Event
	types map[Type]bool
}
type Bus struct {
	mu          sync.Mutex
	subscribers map[*Subscription]subscriber
}

func (b *Bus) Subscribe(ctx context.Context, types ...Type) *Subscription {
	ch := make(chan Event, 256)
	s := &Subscription{Events: ch}
	filter := map[Type]bool{}
	for _, t := range types {
		filter[t] = true
	}
	b.mu.Lock()
	if b.subscribers == nil {
		b.subscribers = map[*Subscription]subscriber{}
	}
	b.subscribers[s] = subscriber{s, ch, filter}
	b.mu.Unlock()
	go func() { <-ctx.Done(); b.mu.Lock(); delete(b.subscribers, s); close(ch); b.mu.Unlock() }()
	return s
}
func (b *Bus) Notify(e Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, s := range b.subscribers {
		if len(s.types) > 0 && !s.types[e.Type] {
			continue
		}
		select {
		case s.ch <- e.Clone():
		default:
			s.sub.Resync.Store(true)
			s.sub.Lost.Add(1)
		}
	}
}
