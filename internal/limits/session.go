package limits

import "github.com/JoaoVictorVM/comporta/internal/storage"

// Session is the logical request behind one or more attempts. It is an
// internal handle, never a bearer value: it counts the request per minute once,
// holds one concurrency slot from the first admission until Close, and keeps
// the policy and generation captured then, so the attempts of one chain never
// mix configurations.
type Session struct {
	policy  Policy
	request Request
	last    *Handle
	closed  bool
}

// ReserveSession admits the first attempt of a logical request that may
// continue with later attempts. Everything else is ReserveSelect.
func (m *Manager) ReserveSession(p Policy, r Request, candidates []Candidate, commit func(storage.Reservation, Selection) (int64, error)) (*Session, *Handle, Decision, Selection, error) {
	if r.AttemptIndex != 1 {
		return nil, nil, Decision{}, Selection{}, ErrIntegrity
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	op := &Session{policy: p, request: r}
	h, decision, selection, err := m.reserveLocked(p, r, candidates, commit, op)
	if h == nil {
		return nil, nil, decision, selection, err
	}
	// An identical repeat returns the operation that made the reservation.
	if h.op != nil {
		op = h.op
	}
	op.last = h
	return op, h, decision, selection, err
}

// ReserveContinuation admits the next attempt of a live operation. It needs the
// previous attempt settled or released durably, the next index in sequence and
// the same request and subject. Rate per minute and concurrency are not checked
// again; tokens, budgets and hard caps apply to the new attempt as usual.
func (m *Manager) ReserveContinuation(op *Session, r Request, candidates []Candidate, commit func(storage.Reservation, Selection) (int64, error)) (*Handle, Decision, Selection, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if op == nil || op.closed || op.last == nil || op.last.state == storage.ReservationReserved {
		return nil, Decision{}, Selection{}, ErrConflict
	}
	first := op.request
	if r.RequestID != first.RequestID || r.Subject != first.Subject || r.Operation != first.Operation ||
		r.AttemptIndex != op.last.request.AttemptIndex+1 || r.AttemptIndex > storage.MaxAttempts {
		return nil, Decision{}, Selection{}, ErrConflict
	}
	r.Generation = first.Generation
	h, decision, selection, err := m.reserveLocked(op.policy, r, candidates, commit, op)
	if h != nil {
		op.last = h
	}
	return h, decision, selection, err
}

// CloseSession frees the concurrency slot of the logical request exactly
// once, whatever ended it: an answer, a refusal, a cancellation or a panic.
// Settlements still pending keep their financial obligation reserved.
func (m *Manager) CloseSession(op *Session) {
	if op == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if op.closed {
		return
	}
	op.closed = true
	if op.last != nil {
		m.releaseSlot(op.request.Subject)
	}
}
