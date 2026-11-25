package proxy

import (
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/config"
)

const (
	fallbackHeader = "x-comporta-fallback"
	// fallbackTotal caps a non-streaming request that has somewhere to fall back
	// to, whatever the listener allows: every attempt shares it.
	fallbackTotal = 120 * time.Second
	// maxFallbackHeader fits five sends of 256-byte names fully escaped.
	maxFallbackHeader = 16 << 10
	maxAttempts       = 5
)

// fallbackChain returns the models that may follow initial, in order. It starts
// after the first occurrence of initial in the app's chain, or at its start
// when initial is not in it. Models in another API format, outside the
// allowlist, undeclared or repeated are left out: they are never sent,
// reserved or reported as failed. Entries in another API format stay: whether
// they can represent the request is decided when the cursor reaches them.
func (o *operation) fallbackChain(app config.App, envelope *Envelope, initial string) []string {
	if envelope.Operation != OperationGeneration || len(app.Fallback) == 0 {
		return nil
	}
	rest := app.Fallback
	if i := slices.Index(rest, initial); i >= 0 {
		rest = rest[i+1:]
	}
	seen := map[string]bool{initial: true}
	var out []string
	for _, model := range rest {
		if seen[model] || !o.eligibleFallback(app, model) {
			continue
		}
		seen[model] = true
		out = append(out, model)
	}
	return out
}

func (o *operation) eligibleFallback(app config.App, model string) bool {
	if !slices.Contains(app.AllowedModels, model) {
		return false
	}
	name, ok := o.snapshot.ProviderForModel(model)
	if !ok {
		return false
	}
	_, ok = o.snapshot.Provider(name)
	return ok
}

// fallbackCause says whether a failed attempt may be retried on the next model,
// and the closed cause that will be recorded: 429, an upstream 5xx status
// (529 included), connection or timeout. Everything decided locally and every
// other provider answer is terminal. A stream error counts only when the parser
// recognized its class.
func fallbackCause(result AttemptResult) (string, bool) {
	if result.Committed || result.ClientDisconnected || result.NotSent {
		return "", false
	}
	switch result.ErrorCode {
	case CodeProviderRateLimited:
		return "429", true
	case CodeProviderOverloaded:
		return strconv.Itoa(statusOverloaded), true
	case CodeProviderUnreachable:
		return "connection", true
	case CodeProviderTimeout:
		return "timeout", true
	case CodeProviderError:
		if status := result.UpstreamStatus; result.ErrorClass == ClassUpstream5xx && status.Known && status.Value >= 500 && status.Value <= 599 {
			return strconv.Itoa(status.Value), true
		}
	}
	return "", false
}

// failedSend is one model that was sent and failed, as reported to the client.
type failedSend struct {
	model string
	cause string
}

func historyText(sends []failedSend) string {
	parts := make([]string, 0, len(sends))
	for _, s := range sends {
		parts = append(parts, boundedText(s.model)+": "+s.cause)
	}
	return strings.Join(parts, ", ")
}

func fallbackExhausted(sends []failedSend) Failure {
	return Failure{http.StatusBadGateway, CodeFallbackExhausted, ClassUpstream5xx, "all fallback models failed (" + historyText(sends) + ")"}
}

func fallbackDeadline(total time.Duration, sends []failedSend) Failure {
	return Failure{http.StatusGatewayTimeout, CodeFallbackDeadlineExceeded, ClassTimeout,
		fmt.Sprintf("fallback deadline of %s exceeded (%s)", total.Round(time.Second), historyText(sends))}
}

func fallbackTargetExhausted(target string) *Rejection {
	return &Rejection{Status: http.StatusServiceUnavailable, Code: CodeFallbackPolicyTargetExhausted, Class: ClassInternal,
		Message: fmt.Sprintf("the on_limit policy requires %q, which already failed in this request; it was not sent again", boundedText(target))}
}

// transition is one admitted switch from a failed model to the next one.
type transition struct {
	from, to, cause string
}

// fallbackHeaderValue lists the admitted transitions in order. Names are
// percent-escaped so the delimiters stay unambiguous; causes are a closed set.
func fallbackHeaderValue(transitions []transition) (string, bool) {
	parts := make([]string, 0, len(transitions))
	for _, t := range transitions {
		parts = append(parts, escapeModel(t.from)+"->"+escapeModel(t.to)+" ("+t.cause+")")
	}
	value := strings.Join(parts, "; ")
	return value, len(value) <= maxFallbackHeader
}

func escapeModel(name string) string {
	var b strings.Builder
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("._:/@+=~-", c) >= 0 {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

// fallbackDeadline is when a non-streaming operation with somewhere to fall
// back to must end: the listener's limit, but never more than 120 s, counted
// from admission and never renewed between attempts.
func fallbackDeadlineAt(admission time.Time, nonStreaming time.Duration) time.Time {
	return admission.Add(min(nonStreaming, fallbackTotal))
}
