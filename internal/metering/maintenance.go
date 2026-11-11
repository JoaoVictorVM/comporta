package metering

import (
	"context"

	"github.com/JoaoVictorVM/comporta/internal/storage"
)

// Maintain materializes every queued completion, then runs fn while no batch
// can be committed. A deletion inside fn therefore never races with a late
// batch that would write the deleted rows back. A failed flush runs nothing.
func (s *Service) Maintain(ctx context.Context, fn func(context.Context) error) error {
	s.flushMu.Lock()
	defer s.flushMu.Unlock()
	if err := s.flushLocked(ctx); err != nil {
		return err
	}
	return fn(ctx)
}

// HasPending reports whether a queued record still belongs to one of the
// identified users.
func (s *Service) HasPending(subjects []storage.SubjectRef) bool {
	selected := make(map[storage.SubjectRef]bool, len(subjects))
	for _, subject := range subjects {
		selected[subject] = true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.pending {
		if !r.Anonymous && selected[storage.SubjectRef{App: r.AppName, Hash: r.UserHash}] {
			return true
		}
	}
	return false
}
