package limits

import (
	"context"
	"errors"

	"github.com/JoaoVictorVM/comporta/internal/storage"
)

// ErrPaused means the shared control state refused the admission because an
// operator paused the app or every app. Only a coordinator that reads pause
// state durably returns it; the local one leaves pauses to runtimecontrol.
var ErrPaused = errors.New("app is paused by an operator")

// Commit makes an admission durable together with the usage journal. The
// context carries whatever transaction the coordinator opened, so the write
// joins it.
type Commit func(ctx context.Context, res storage.Reservation, sel Selection) (int64, error)

// Persist makes a settlement durable, normally with the usage result.
type Persist func(ctx context.Context, st storage.Settlement) (int64, error)

// Attempt is an admitted attempt as consumers see it. It is never a bearer
// value and never comes from a client.
type Attempt interface {
	Degraded() bool
	Selection() Selection
}

// Chain is a logical request that may span several attempts.
type Chain interface {
	chain()
}

// Coordinator is the admission and settlement strategy. The local Manager
// decides in memory with SQLite; a distributed one decides on shared state.
// Both keep the same contract: nothing is sent before an admission returns,
// a refusal reserves nothing, and an obligation whose settlement could not be
// confirmed stays reserved.
type Coordinator interface {
	ReserveAttempt(ctx context.Context, p Policy, r Request, candidates []Candidate, commit Commit) (Attempt, Decision, Selection, error)
	OpenChain(ctx context.Context, p Policy, r Request, candidates []Candidate, commit Commit) (Chain, Attempt, Decision, Selection, error)
	ContinueChain(ctx context.Context, op Chain, r Request, candidates []Candidate, commit Commit) (Attempt, Decision, Selection, error)
	CloseChain(op Chain)
	SettleAttempt(ctx context.Context, a Attempt, u Usage, persist Persist) error
	ReleaseAttempt(ctx context.Context, a Attempt, reason ReleaseReason, persist Persist) error
}

func (*Session) chain() {}

func bind(ctx context.Context, commit Commit) func(storage.Reservation, Selection) (int64, error) {
	return func(res storage.Reservation, sel Selection) (int64, error) { return commit(ctx, res, sel) }
}

func bindPersist(ctx context.Context, persist Persist) func(storage.Settlement) (int64, error) {
	if persist == nil {
		return nil
	}
	return func(st storage.Settlement) (int64, error) { return persist(ctx, st) }
}

// attempt avoids a typed nil inside a non-nil interface.
func attempt(h *Handle) Attempt {
	if h == nil {
		return nil
	}
	return h
}

func (m *Manager) ReserveAttempt(ctx context.Context, p Policy, r Request, candidates []Candidate, commit Commit) (Attempt, Decision, Selection, error) {
	h, d, s, err := m.ReserveSelect(p, r, candidates, bind(ctx, commit))
	return attempt(h), d, s, err
}

func (m *Manager) OpenChain(ctx context.Context, p Policy, r Request, candidates []Candidate, commit Commit) (Chain, Attempt, Decision, Selection, error) {
	op, h, d, s, err := m.ReserveSession(p, r, candidates, bind(ctx, commit))
	if op == nil {
		return nil, attempt(h), d, s, err
	}
	return op, attempt(h), d, s, err
}

func (m *Manager) ContinueChain(ctx context.Context, op Chain, r Request, candidates []Candidate, commit Commit) (Attempt, Decision, Selection, error) {
	session, _ := op.(*Session)
	h, d, s, err := m.ReserveContinuation(session, r, candidates, bind(ctx, commit))
	return attempt(h), d, s, err
}

func (m *Manager) CloseChain(op Chain) {
	if session, ok := op.(*Session); ok {
		m.CloseSession(session)
	}
}

func (m *Manager) SettleAttempt(ctx context.Context, a Attempt, u Usage, persist Persist) error {
	h, _ := a.(*Handle)
	return m.Settle(h, u, bindPersist(ctx, persist))
}

func (m *Manager) ReleaseAttempt(ctx context.Context, a Attempt, reason ReleaseReason, persist Persist) error {
	h, _ := a.(*Handle)
	return m.Release(h, reason, bindPersist(ctx, persist))
}
