package limits

import (
	"github.com/JoaoVictorVM/comporta/internal/storage"
)

// ForgetSubjects removes end users from the projection together with their
// persisted deletion. persist runs under the coordinator lock, so no admission,
// settlement or checkpoint of these users can interleave with it; memory changes
// only after it succeeds, and those changes cannot fail. App buckets, app rate
// windows and every reservation of the app stay as they are, so forgetting a
// user never reopens the app's budget.
//
// A user that still holds a reservation, or a settlement waiting for its retry,
// has an unreconciled obligation: the call refuses with storage.ErrBusy and
// changes nothing.
func (m *Manager) ForgetSubjects(subjects []storage.SubjectRef, persist func() error) (cleared int, err error) {
	selected := make(map[Subject]bool, len(subjects))
	for _, s := range subjects {
		selected[Subject{App: s.App, Hash: s.Hash}] = true
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, h := range m.handles {
		if selected[h.request.Subject] {
			return 0, storage.BusyError{Reason: "open limit reservation"}
		}
	}
	for _, pending := range m.retry {
		if selected[pending.handle.request.Subject] {
			return 0, storage.BusyError{Reason: "settlement awaiting retry"}
		}
	}
	if err := persist(); err != nil {
		return 0, err
	}
	clearedSubjects := map[Subject]bool{}
	for key := range m.buckets {
		s := Subject{App: key.app, Hash: key.subject, Anonymous: key.anonymous}
		if key.scope == ScopeUser && selected[s] {
			delete(m.buckets, key)
			clearedSubjects[s] = true
		}
	}
	removed := m.removed[:0]
	for _, key := range m.removed {
		if key.scope == ScopeUser && selected[Subject{App: key.app, Hash: key.subject, Anonymous: key.anonymous}] {
			continue
		}
		removed = append(removed, key)
	}
	m.removed = removed
	for s := range selected {
		if _, ok := m.windows[s]; ok {
			delete(m.windows, s)
			clearedSubjects[s] = true
		}
		if _, ok := m.concurrent[s]; ok {
			delete(m.concurrent, s)
			clearedSubjects[s] = true
		}
	}
	return len(clearedSubjects), nil
}

// Apps lists the apps the projection holds state for.
func (m *Manager) Apps() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	seen := map[string]bool{}
	for key := range m.buckets {
		seen[key.app] = true
	}
	for s := range m.windows {
		seen[s.App] = true
	}
	for _, h := range m.handles {
		seen[h.app] = true
	}
	apps := make([]string, 0, len(seen))
	for app := range seen {
		apps = append(apps, app)
	}
	return apps
}
