package metering

import (
	"context"
	"errors"
	"github.com/JoaoVictorVM/comporta/internal/storage"
	"time"
)

type timer interface {
	C() <-chan time.Time
	Reset(time.Duration)
	Stop()
}
type systemTimer struct{ *time.Timer }

func (t systemTimer) C() <-chan time.Time   { return t.Timer.C }
func (t systemTimer) Reset(d time.Duration) { t.Timer.Reset(d) }
func (t systemTimer) Stop()                 { t.Timer.Stop() }
func newSystemTimer(d time.Duration) timer  { return systemTimer{time.NewTimer(d)} }

func (s *Service) Flush(ctx context.Context) error {
	s.flushMu.Lock()
	defer s.flushMu.Unlock()
	return s.flushLocked(ctx)
}

func (s *Service) flushLocked(ctx context.Context) error {
	for {
		s.mu.Lock()
		n := min(1000, len(s.pending))
		batch := append([]storage.Record(nil), s.pending[:n]...)
		s.mu.Unlock()
		if n == 0 {
			s.mu.Lock()
			broken := s.unhealthy
			s.mu.Unlock()
			if broken {
				return storage.ErrUnavailable
			}
			return nil
		}
		if err := s.store.Commit(ctx, batch); err != nil {
			if errors.Is(err, storage.ErrInvalid) || errors.Is(err, storage.ErrConflict) {
				s.mu.Lock()
				s.unhealthy = true
				s.mu.Unlock()
			}
			return err
		}
		s.mu.Lock()
		s.pending = s.pending[n:]
		s.mu.Unlock()
		for range n {
			<-s.slots
		}
	}
}

func (s *Service) Run(ctx context.Context) error {
	tick := s.newTimer(2 * time.Second)
	defer tick.Stop()
	maintenance := s.newTimer(time.Hour)
	defer maintenance.Stop()
	delay := time.Second
	retrying := false
	for {
		wake := s.wake
		if retrying {
			wake = nil
		}
		select {
		case <-ctx.Done():
			shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			err := s.Flush(shutdown)
			if err == nil && len(s.slots) != 0 {
				err = storage.ErrUnavailable
			}
			if err == nil {
				if store, ok := s.store.(interface{ Checkpoint(context.Context) error }); ok {
					err = store.Checkpoint(shutdown)
				}
			}
			cancel()
			return err
		case <-maintenance.C():
			if err := s.store.Retain(ctx, s.now().Add(-time.Duration(s.retention())*24*time.Hour)); err != nil {
				s.warnStorage(err)
			}
			maintenance.Reset(time.Hour)
			continue
		case <-wake:
		case <-tick.C():
		}
		if err := s.Flush(ctx); err != nil {
			s.warnStorage(err)
			retrying = true
			tick.Reset(delay)
			delay = min(30*time.Second, delay*2)
		} else {
			retrying = false
			delay = time.Second
			tick.Reset(2 * time.Second)
		}
	}
}
