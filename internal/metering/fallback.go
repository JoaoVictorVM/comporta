package metering

import (
	"context"
	"errors"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/limits"
	"github.com/JoaoVictorVM/comporta/internal/pricing"
	"github.com/JoaoVictorVM/comporta/internal/proxy"
	"github.com/JoaoVictorVM/comporta/internal/storage"
)

var errNotConcluded = errors.New("previous attempt not concluded")

// session is the accounting of one logical request that may fall back to other
// models. Every attempt has its own quote, reservation and journal entry. The
// result of the last concluded attempt waits here until the next admission
// makes it intermediate or Close makes it terminal, so the aggregates count
// the request exactly once.
type session struct {
	service  *Service
	snapshot *config.Snapshot
	op       limits.Chain
	first    storage.Record
	current  storage.Record
	quote    pricing.Quote
	handle   limits.Attempt
	done     *storage.Record
	failed   error
	closed   bool
}

// AdmitSession admits the first attempt of a logical request. Continuations
// need the limit coordinator and a store that admits them durably; without
// them the admission is a plain one.
func (s *Service) AdmitSession(ctx context.Context, snapshot *config.Snapshot, e *proxy.Envelope, candidates []proxy.Candidate) (proxy.AccountingSession, proxy.Admission, *proxy.Rejection) {
	if _, durable := s.limitStore.(storage.FallbackStore); s.limits == nil || !durable || e.Operation != proxy.OperationGeneration {
		admission, rejection := s.AdmitCandidates(ctx, snapshot, e, candidates)
		return nil, admission, rejection
	}
	open := &session{}
	admission, rejection, stale := s.admit(ctx, snapshot, e, candidates, open)
	if stale {
		rejection := *errReloaded
		rejection.Stale = true
		return nil, proxy.Admission{}, &rejection
	}
	if rejection != nil {
		return nil, proxy.Admission{}, rejection
	}
	return open, admission, nil
}

func (x *session) Conclude(result proxy.AttemptResult) error {
	if x.done != nil || x.closed {
		return errNotConcluded
	}
	record, ok, err := x.service.conclude(x.current, x.quote, x.handle, result)
	if !ok {
		x.failed = errNotConcluded
		return x.failed
	}
	x.done, x.failed = &record, err
	return err
}

// Continue admits the next attempt on the snapshot captured at the first
// admission. A refusal never records a block: the previous attempt stays the
// terminal one, with the refusal as the answer of the logical request.
func (x *session) Continue(ctx context.Context, e *proxy.Envelope, candidates []proxy.Candidate, from, cause string) (proxy.Admission, *proxy.Rejection) {
	s := x.service
	reject := func(status int, code proxy.Code, message string) (proxy.Admission, *proxy.Rejection) {
		class := proxy.ClassInvalidRequest
		if status >= 500 {
			class = proxy.ClassInternal
		}
		return proxy.Admission{}, &proxy.Rejection{Status: status, Code: code, Class: class, Message: message}
	}
	if x.closed || x.done == nil || x.failed != nil || x.current.AttemptIndex >= storage.MaxAttempts {
		return reject(503, proxy.CodeUsageStorageUnavailable, storage.ErrUnavailable.Error())
	}
	if len(candidates) == 0 || len(candidates) > 2 || candidates[0].ID != proxy.CandidateOriginal || candidates[0].Unavailable != nil ||
		(len(candidates) == 2 && candidates[1].ID != proxy.CandidateDowngrade) {
		return reject(500, proxy.CodeInternalError, "internal error")
	}
	base, rejection := s.price(x.snapshot, e, candidates[0])
	if rejection != nil {
		return proxy.Admission{}, rejection
	}
	var target *priced
	var targetRefusal *proxy.Rejection
	if len(candidates) == 2 {
		targetRefusal = candidates[1].Unavailable
		if targetRefusal == nil {
			quoted, refusal := s.price(x.snapshot, e, candidates[1])
			if refusal == nil {
				target = &quoted
			}
			targetRefusal = refusal
		}
	}
	s.mu.Lock()
	bad := s.unhealthy
	s.mu.Unlock()
	if bad {
		return reject(503, proxy.CodeUsageStorageUnavailable, storage.ErrUnavailable.Error())
	}
	select {
	case s.slots <- struct{}{}:
	default:
		return reject(503, proxy.CodeUsageStorageUnavailable, storage.ErrUnavailable.Error())
	}
	release := func() { <-s.slots }
	now := s.now().UTC()
	if now.Before(x.current.StartedAt) {
		now = x.current.StartedAt
	}
	location, err := time.LoadLocation(x.first.Timezone)
	if err != nil {
		release()
		return reject(503, proxy.CodeUsageStorageUnavailable, storage.ErrUnavailable.Error())
	}
	index := x.current.AttemptIndex + 1
	r := x.first
	r.AttemptIndex, r.StartedAt, r.FinishedAt, r.Day = index, now, time.Time{}, now.In(location).Format("2006-01-02")
	r.Provider, r.ModelRequested, r.ModelUsed = base.candidate.ProviderName, e.ModelRequested, base.candidate.Model
	r.Outcome, r.ReservedCost, r.Quote, r.RequestCount = "fallback", base.reserved, base.frozen, 1
	r.PolicyOutcome, r.PolicyLimitName, r.PolicyScope, r.PolicyReason, r.PolicyRenewsAt = "", "", "", "", nil
	r.LimitsDegraded, r.FallbackFrom, r.FallbackCause = false, from, cause
	request := limits.Request{RequestID: r.RequestID, AttemptIndex: index, Subject: limits.Subject{App: r.AppName, Hash: r.UserHash, Anonymous: r.Anonymous},
		Operation: limits.OperationGeneration, Generation: x.snapshot.Generation()}
	options := []limits.Candidate{{ID: limits.CandidateOriginal, Model: base.candidate.Model, EstimatedTokens: base.tokens, ReservedCost: base.reserved}}
	if len(candidates) == 2 {
		option := limits.Candidate{ID: limits.CandidateDowngrade, Model: candidates[1].Model, Unavailable: target == nil}
		if target != nil {
			option.EstimatedTokens, option.ReservedCost = target.tokens, target.reserved
		}
		options = append(options, option)
	}
	chosen := func(sel limits.Selection) priced {
		if sel.Candidate == limits.CandidateDowngrade && target != nil {
			return *target
		}
		return base
	}
	admitted := func(sel limits.Selection) storage.Record {
		out := withPolicy(r, chosen(sel), sel)
		out.Outcome = "fallback"
		return out
	}
	fallbacks := s.limitStore.(storage.FallbackStore)
	operation := storage.Operation{RequestID: r.RequestID, AppName: r.AppName, Subject: r.UserHash, Anonymous: r.Anonymous, Generation: x.first.Quote.Generation, StartedAt: x.first.StartedAt}
	handle, decision, sel, err := s.limits.ContinueChain(ctx, x.op, request, options, func(ctx context.Context, res storage.Reservation, sel limits.Selection) (int64, error) {
		out := admitted(sel)
		out.LimitsDegraded = res.Quote.Degraded
		return fallbacks.AdmitContinuation(ctx, out, res, operation)
	})
	switch {
	case errors.Is(err, limits.ErrPaused):
		release()
		return proxy.Admission{}, pausedRejection(r.AppName)
	case errors.Is(err, limits.ErrUnavailable):
		release()
		return reject(503, proxy.CodeLimitsUnavailable, limits.ErrUnavailable.Error())
	case errors.Is(err, limits.ErrTargetUnavailable):
		release()
		if targetRefusal != nil {
			return proxy.Admission{}, targetRefusal
		}
		return reject(503, proxy.CodeInternalError, "internal error")
	case errors.Is(err, limits.ErrIntegrity), errors.Is(err, limits.ErrConflict), errors.Is(err, limits.ErrStale):
		release()
		s.logger.Error("limit state integrity failure; fallback attempt rejected")
		return reject(503, proxy.CodeInternalError, "internal error")
	case err != nil:
		s.warnStorage(err)
		release()
		return reject(503, proxy.CodeUsageStorageUnavailable, storage.ErrUnavailable.Error())
	case !decision.Allowed:
		release()
		v := *decision.Primary
		code := limitCode(v)
		refusal := &proxy.Rejection{Status: 429, Code: code, Class: proxy.ClassRateLimit, Message: limits.Message(v, r.AppName, now),
			Limit: &proxy.LimitDetail{LimitName: v.LimitName, Scope: string(v.Scope), Reason: string(v.Reason), RenewsAt: v.RenewsAt}}
		if v.Kind == limits.KindRate {
			refusal.RetryAfter = v.RetryAfter
		}
		return proxy.Admission{}, refusal
	}
	// The next attempt is durable: the previous one is now intermediate.
	previous := *x.done
	previous.RequestCount = 0
	s.enqueue(previous)
	next := admitted(sel)
	next.LimitsDegraded = handle.Degraded()
	x.current, x.quote, x.handle, x.done = next, chosen(sel).quote, handle, nil
	return proxy.Admission{Budget: budgetHeaders(decision), Selected: proxy.CandidateID(sel.Candidate), Policy: proxy.PolicyOutcome(sel.Outcome)}, nil
}

// Close makes the last concluded attempt terminal and frees the concurrency
// slot of the logical request.
func (x *session) Close(terminal *proxy.Failure) {
	if x.closed {
		return
	}
	x.closed = true
	if x.done != nil {
		final := *x.done
		final.RequestCount = 1
		if terminal != nil {
			own := 0
			if final.DownstreamStatus != nil {
				own = *final.DownstreamStatus
			}
			if string(terminal.Code) != final.ErrorCode || terminal.Status != own {
				status := terminal.Status
				final.TerminalErrorCode, final.TerminalStatus = string(terminal.Code), &status
			}
		}
		x.service.enqueue(final)
	}
	x.service.limits.CloseChain(x.op)
}
