package loopguard

import (
	"context"
	"time"
)

const (
	// minSweepInterval bounds how often the worker walks the subjects, so a
	// burst of deadlines does not turn the sweep into a busy loop.
	minSweepInterval = time.Second
	// reconcileInterval is how long the worker waits without any deadline
	// before checking the configuration for removed or disabled apps.
	reconcileInterval = 10 * time.Second
)

// Run releases expired state and reconciles the configuration until ctx ends.
// On return the state is dropped: a restart reopens every window.
func (m *Manager) Run(ctx context.Context) error {
	timer := m.clock.NewTimer(reconcileInterval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			m.mu.Lock()
			m.subjects = map[Subject]*subjectState{}
			m.apps = map[string]*appState{}
			m.sweeping = false
			m.mu.Unlock()
			m.occurrences.Store(0)
			return nil
		case <-timer.C():
		case <-m.wake:
		}
		m.Reconcile()
		now := m.clock.Elapsed()
		m.mu.Lock()
		next, found := m.sweepLocked(now)
		m.sweepAt, m.sweeping = next, found
		m.mu.Unlock()
		wait := reconcileInterval
		if found {
			wait = min(max(next-now, minSweepInterval), reconcileInterval)
		}
		timer.Reset(wait)
	}
}

// Reconcile applies the current configuration to every app with state, so a
// removed or disabled app is cleared even without traffic.
func (m *Manager) Reconcile() {
	if m.source == nil {
		return
	}
	snapshot := m.source.Capture()
	if snapshot == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for name, app := range m.apps {
		if snapshot.Generation() < app.generation {
			continue
		}
		policy, ok := PolicyFor(snapshot, name)
		if !ok {
			if !app.removed {
				m.dropAppLocked(name)
			}
			app.generation, app.removed, app.policy = snapshot.Generation(), true, Policy{}
			continue
		}
		_, _ = m.applyLocked(name, policy)
	}
}

// Apps lists the apps with a subject in memory, including apps already removed
// from the configuration whose state the worker has not cleared yet.
func (m *Manager) Apps() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	seen := map[string]bool{}
	for subject := range m.subjects {
		seen[subject.App] = true
	}
	apps := make([]string, 0, len(seen))
	for app := range seen {
		apps = append(apps, app)
	}
	return apps
}

// ForgetSubjects drops counters and cooldowns of identified users. It cannot
// fail and calls nothing outside the guard, so a deletion that already
// committed its history always completes here.
func (m *Manager) ForgetSubjects(subjects []Subject) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	cleared := 0
	for _, subject := range subjects {
		if state := m.subjects[subject]; state != nil {
			m.removeLocked(subject, state)
			cleared++
		}
	}
	return cleared
}

// Occurrences reports the instants and digests held, for tests and benchmarks.
func (m *Manager) Occurrences() int64 { return m.occurrences.Load() }

// Subjects reports how many subjects are in memory.
func (m *Manager) Subjects() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.subjects)
}
