package proxy

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/sensitivedata"
	"github.com/JoaoVictorVM/comporta/internal/telemetry"
	"github.com/JoaoVictorVM/comporta/internal/useridentity"
)

const sinkTimeout = time.Second

// AttemptResult describes one upstream attempt. It holds counters and categories
// only: no content, no raw user identifier, no raw error. UserHash is what later stages
// key per-user data by, always together with AppName and Anonymous.
type AttemptResult struct {
	NotSent            bool
	RequestID          string
	AttemptIndex       int
	AppName            string
	KeyID              string
	UserHash           useridentity.Hash
	Anonymous          bool
	Operation          Operation
	Provider           string
	ModelRequested     string
	ModelUsed          string
	UpstreamStatus     Optional[int]
	DownstreamStatus   Optional[int]
	ErrorClass         ErrorClass
	ErrorCode          Code
	StartedAt          time.Time
	Latency            time.Duration
	UpstreamHeaders    Optional[time.Duration]
	UpstreamTTFB       Optional[time.Duration]
	ClientTTFB         Optional[time.Duration]
	Usage              Usage
	UsageComplete      bool
	TerminalSeen       bool
	Committed          bool
	ClientDisconnected bool
	// CountedInputTokens is the advisory answer of count_tokens, kept apart from
	// the consumption counters so that it can never be charged as usage.
	CountedInputTokens Optional[int64]
	// Detections is the safe summary of the request's inspection, shared by every
	// attempt of the request.
	Detections sensitivedata.Summary
	// CacheHit marks an answer replayed from the response cache. Only the replay
	// stage sets it, always with NotSent: nothing reached a provider and nothing
	// new was consumed.
	CacheHit bool
}

// ResultSink receives each result once. The default keeps nothing; it is not a
// durable queue, and a persistent consumer must define its own backpressure.
type ResultSink interface {
	Publish(ctx context.Context, result AttemptResult) error
}

type discardSink struct{}

func (discardSink) Publish(context.Context, AttemptResult) error { return nil }

// attempt owns the result of one upstream attempt.
type attempt struct {
	complete  func(AttemptResult)
	result    AttemptResult
	admission time.Time
	began     time.Time
	now       func() time.Time
	sink      ResultSink
	logger    *slog.Logger
	once      sync.Once
	// tel and trace observe the attempt; nil observes nothing.
	tel   *telemetry.Op
	trace *telemetry.Attempt
}

func newAttempt(envelope *Envelope, provider string, admission time.Time, now func() time.Time, sink ResultSink, logger *slog.Logger) *attempt {
	began := now()
	return &attempt{
		result: AttemptResult{
			RequestID:      envelope.RequestID,
			AttemptIndex:   1,
			AppName:        envelope.AppName,
			KeyID:          envelope.KeyID,
			UserHash:       envelope.Identity.Hash(),
			Anonymous:      envelope.Identity.Anonymous(),
			Operation:      envelope.Operation,
			Provider:       provider,
			ModelRequested: envelope.ModelRequested,
			ModelUsed:      envelope.ModelEffective,
			StartedAt:      began.UTC(),
			Detections:     envelope.Sensitive.Clone(),
		},
		admission: admission,
		began:     began,
		now:       now,
		sink:      sink,
		logger:    logger,
	}
}

func (a *attempt) fail(failure Failure) {
	if a.result.ErrorCode == "" {
		a.result.ErrorClass, a.result.ErrorCode = failure.Class, failure.Code
	}
}

func (a *attempt) clientGone() {
	a.result.ClientDisconnected = true
	a.fail(Failure{Code: CodeClientDisconnected, Class: ClassClientCancel})
}

func (a *attempt) markUpstreamHeaders(status int) {
	a.result.UpstreamStatus = known(status)
	a.result.UpstreamHeaders = known(a.now().Sub(a.began))
}

func (a *attempt) markUpstreamByte() {
	if !a.result.UpstreamTTFB.Known {
		a.result.UpstreamTTFB = known(a.now().Sub(a.began))
	}
}

func (a *attempt) markClientByte(status int) {
	a.result.Committed = true
	a.result.DownstreamStatus = known(status)
	if !a.result.ClientTTFB.Known {
		a.result.ClientTTFB = known(a.now().Sub(a.admission))
	}
}

// observe hands telemetry a projection of the result. The user hash, key ID,
// request ID and detections never leave here.
func (a *attempt) observe(result AttemptResult, ended time.Time) {
	if a.tel == nil {
		return
	}
	if result.Committed && result.ClientTTFB.Known && result.DownstreamStatus.Known && result.DownstreamStatus.Value < 400 {
		a.tel.Delivered(result.ClientTTFB.Value)
	}
	end := telemetry.AttemptEnd{
		Sent: !result.NotSent, Result: attemptOutcome(result), ErrorType: string(result.ErrorCode), Ended: ended,
		InputTokens: optionalOf(result.Usage.Input), OutputTokens: optionalOf(result.Usage.Output),
		CacheReadTokens: optionalOf(result.Usage.CacheRead), CacheWriteTokens: optionalOf(result.Usage.CacheWrite),
	}
	if result.UpstreamStatus.Known {
		end.Status = telemetry.Some(result.UpstreamStatus.Value)
	}
	a.trace.Finish(end)
}

func optionalOf(v Optional[int64]) telemetry.Optional[int64] {
	return telemetry.Optional[int64]{Value: v.Value, Known: v.Known}
}

// attemptOutcome classifies what the provider did, apart from what the gateway
// did with the answer afterwards.
func attemptOutcome(r AttemptResult) telemetry.AttemptResult {
	switch {
	case r.ClientDisconnected || r.ErrorClass == ClassClientCancel:
		return telemetry.AttemptCanceled
	case !r.UpstreamStatus.Known:
		return telemetry.AttemptTransportError
	case r.UpstreamStatus.Value >= 500:
		return telemetry.AttemptServerError
	case r.UpstreamStatus.Value >= 400:
		return telemetry.AttemptClientError
	case r.UpstreamStatus.Value >= 300:
		return telemetry.AttemptServerError
	}
	switch r.ErrorClass {
	case ClassConnection, ClassTimeout, ClassProtocol:
		return telemetry.AttemptTransportError
	case ClassUpstream5xx, ClassOverload:
		return telemetry.AttemptServerError
	}
	return telemetry.AttemptOK
}

// finish publishes the result once, whatever path ended the attempt. It runs on its
// own context because the client's is often the reason the attempt ended, and a sink
// that misbehaves must neither hold the request nor take the process down.
func (a *attempt) finish() {
	a.once.Do(func() {
		ended := a.now()
		a.result.Latency = ended.Sub(a.began)
		result := a.result
		if a.complete != nil {
			stage := a.tel.StageAttempt(telemetry.StageSettle, result.AttemptIndex)
			a.complete(result)
			stage.End()
		}
		a.observe(result, ended)
		ctx, cancel := context.WithTimeout(context.Background(), sinkTimeout)
		defer cancel()
		done := make(chan struct{})
		go func() {
			defer close(done)
			defer func() {
				if recover() != nil {
					a.logger.Error("the attempt result consumer failed")
				}
			}()
			if err := a.sink.Publish(ctx, result); err != nil {
				a.logger.Error("the attempt result consumer failed")
			}
		}()
		select {
		case <-done:
		case <-ctx.Done():
			a.logger.Error("the attempt result consumer timed out")
		}
	})
}
