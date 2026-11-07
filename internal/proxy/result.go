package proxy

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

const sinkTimeout = time.Second

// AttemptResult describes one upstream attempt. It holds counters and categories
// only: no content, no user identifier, no raw error.
type AttemptResult struct {
	RequestID          string
	AttemptIndex       int
	AppName            string
	KeyID              string
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
	result    AttemptResult
	admission time.Time
	began     time.Time
	now       func() time.Time
	sink      ResultSink
	logger    *slog.Logger
	once      sync.Once
}

func newAttempt(envelope *Envelope, provider string, admission time.Time, now func() time.Time, sink ResultSink, logger *slog.Logger) *attempt {
	began := now()
	return &attempt{
		result: AttemptResult{
			RequestID:      envelope.RequestID,
			AttemptIndex:   1,
			AppName:        envelope.AppName,
			KeyID:          envelope.KeyID,
			Operation:      envelope.Operation,
			Provider:       provider,
			ModelRequested: envelope.ModelRequested,
			ModelUsed:      envelope.ModelEffective,
			StartedAt:      began.UTC(),
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

// finish publishes the result once, whatever path ended the attempt. It runs on its
// own context because the client's is often the reason the attempt ended, and a sink
// that misbehaves must neither hold the request nor take the process down.
func (a *attempt) finish() {
	a.once.Do(func() {
		a.result.Latency = a.now().Sub(a.began)
		result := a.result
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
