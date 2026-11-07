package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type countingSink struct {
	calls atomic.Int64
	run   func(ctx context.Context) error
}

func (s *countingSink) Publish(ctx context.Context, _ AttemptResult) error {
	s.calls.Add(1)
	if s.run != nil {
		return s.run(ctx)
	}
	return nil
}

func newBareAttempt(sink ResultSink, logs *strings.Builder) *attempt {
	logger := slog.New(slog.DiscardHandler)
	if logs != nil {
		logger = slog.New(slog.NewJSONHandler(logs, nil))
	}
	return newAttempt(&Envelope{RequestID: "r", AppName: "a", KeyID: "k", ModelEffective: "m", rawUser: rawUserCanary, UserSource: UserSourceField},
		"p", time.Now(), time.Now, sink, logger)
}

func TestFinalizationExactlyOnce(t *testing.T) {
	sink := &countingSink{}
	current := newBareAttempt(sink, nil)
	current.finish()
	current.finish()
	if got := sink.calls.Load(); got != 1 {
		t.Fatalf("published %d times", got)
	}
}

func TestFirstFailureWins(t *testing.T) {
	current := newBareAttempt(discardSink{}, nil)
	current.fail(Failure{Code: CodeProviderTimeout, Class: ClassTimeout})
	current.fail(Failure{Code: CodeInternalError, Class: ClassInternal})
	if current.result.ErrorCode != CodeProviderTimeout {
		t.Fatalf("code %s", current.result.ErrorCode)
	}
}

func TestCancelledRequestStillPublishesResult(t *testing.T) {
	_, server := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(deltaChunk))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	var seen atomic.Value
	fx := newFixture(t, server.URL, fixtureOptions{})
	fx.handler.sink = &contextSink{seen: &seen}
	response, err := http.DefaultClient.Do(fx.request(http.MethodPost, "/v1/chat/completions", "openai", streamOpenAIBody))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bufio.NewReader(response.Body).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	deadline := time.Now().Add(5 * time.Second)
	for seen.Load() == nil && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := seen.Load(); got != "live" {
		t.Fatalf("the sink context was %v", got)
	}
}

type contextSink struct{ seen *atomic.Value }

func (s *contextSink) Publish(ctx context.Context, _ AttemptResult) error {
	if ctx.Err() == nil {
		s.seen.Store("live")
	} else {
		s.seen.Store("canceled")
	}
	return nil
}

func TestSinkFailureCannotPanicHandler(t *testing.T) {
	cases := map[string]func(ctx context.Context) error{
		"panic": func(context.Context) error { panic(rawUserCanary) },
		"error": func(context.Context) error { return errors.New(rawUserCanary) },
	}
	for name, run := range cases {
		t.Run(name, func(t *testing.T) {
			var logs strings.Builder
			current := newBareAttempt(&countingSink{run: run}, &logs)
			current.finish()
			if !strings.Contains(logs.String(), "the attempt result consumer failed") {
				t.Fatalf("logs %s", logs.String())
			}
			if strings.Contains(logs.String(), rawUserCanary) {
				t.Fatalf("raw detail logged: %s", logs.String())
			}
		})
	}
	t.Run("a stuck sink does not hold the attempt", func(t *testing.T) {
		release := make(chan struct{})
		defer close(release)
		var logs strings.Builder
		current := newBareAttempt(&countingSink{run: func(context.Context) error {
			<-release
			return nil
		}}, &logs)
		began := time.Now()
		current.finish()
		if elapsed := time.Since(began); elapsed > 2*time.Second {
			t.Fatalf("held for %s", elapsed)
		}
		if !strings.Contains(logs.String(), "timed out") {
			t.Fatalf("logs %s", logs.String())
		}
	})
}

func TestResultContainsNoContent(t *testing.T) {
	const canary = "canary-content"
	_, server := startUpstream(t, sseReply("data: {\"choices\":[{\"delta\":{\"content\":\""+canary+"\"}}]}\n\n"+usageChunk+doneChunk))
	fx := newFixture(t, server.URL, fixtureOptions{})
	fx.post("/v1/chat/completions", "openai",
		`{"model":"fake-gpt","messages":[{"role":"user","content":"`+canary+`"}],"stream":true,"user":"`+rawUserCanary+`"}`)
	result := fx.sink.wait(t)
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), canary) || strings.Contains(string(data), rawUserCanary) {
		t.Fatalf("result holds content: %s", data)
	}
	if result.AppName != "bot" || result.AttemptIndex != 1 || result.RequestID == "" || result.StartedAt.Location() != time.UTC {
		t.Fatalf("result %+v", result)
	}
}
