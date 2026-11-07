package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/auth"
)

const (
	maxResponseBytes  = 64_000_000
	sseReadBuffer     = 32 << 10
	terminalDrainWait = 250 * time.Millisecond
	terminalDrainMax  = 64 << 10
)

// errIdleTimeout is the cancellation cause used when a stream goes silent.
var errIdleTimeout = errors.New("stream idle timeout")

var safeHeaderValue = regexp.MustCompile(`^[\x20-\x7e]{1,256}$`)

// copyResponseHeaders passes only headers an SDK legitimately needs. Prefixes are
// matched against known families, so Set-Cookie, Location and hop-by-hop headers
// can never come through.
func copyResponseHeaders(dst, src http.Header) {
	named := connectionNamed(src)
	for name, values := range src {
		if named[name] || !forwardableResponseHeader(name) {
			continue
		}
		for _, value := range values {
			if safeHeaderValue.MatchString(value) {
				dst.Add(name, value)
			}
		}
	}
}

func forwardableResponseHeader(name string) bool {
	lower := strings.ToLower(name)
	switch lower {
	case "request-id", "x-request-id":
		return true
	}
	return strings.HasPrefix(lower, "anthropic-ratelimit-") || strings.HasPrefix(lower, "x-ratelimit-")
}

func validRetryAfter(value string) bool {
	if seconds, err := strconv.Atoi(value); err == nil {
		return seconds >= 0
	}
	_, err := http.ParseTime(value)
	return err == nil
}

func upstreamStatusFailure(status int, provider string) Failure {
	message := fmt.Sprintf("provider %s returned HTTP %d", provider, status)
	switch {
	case status == http.StatusTooManyRequests:
		return Failure{status, CodeProviderRateLimited, ClassRateLimit, "provider " + provider + " rate limit reached"}
	case status == statusOverloaded:
		return Failure{status, CodeProviderOverloaded, ClassOverload, "provider " + provider + " is overloaded"}
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return Failure{status, CodeProviderError, ClassAuthentication, message}
	case status >= 400 && status < 500:
		return Failure{status, CodeProviderError, ClassInvalidRequest, message}
	case status >= 500 && status < 600:
		return Failure{status, CodeProviderError, ClassUpstream5xx, message}
	case status >= 300 && status < 400:
		return Failure{http.StatusBadGateway, CodeProviderRedirect, ClassProtocol, "provider " + provider + " answered with a redirect, which is not followed"}
	}
	return Failure{http.StatusBadGateway, CodeInvalidUpstreamResponse, ClassProtocol, message}
}

// call is the state of one upstream exchange seen by the response stages.
type call struct {
	handler  *Handler
	w        *trackedWriter
	format   auth.Format
	envelope *Envelope
	attempt  *attempt
	provider string
	// clientCtx ends when the client leaves; upstreamCtx also ends on our deadlines.
	clientCtx   context.Context
	upstreamCtx context.Context
	cancelCause context.CancelCauseFunc
	// writeLimit bounds each write to the client; for a stream it is also how long
	// the provider may stay silent.
	writeLimit time.Duration
}

// failBeforeCommit answers with an error and records it on the attempt.
func (c *call) failBeforeCommit(failure Failure, retryAfter string) {
	c.attempt.fail(failure)
	if retryAfter != "" {
		c.w.Header().Set("Retry-After", retryAfter)
	}
	c.w.prepareWrite(c.writeLimit)
	WriteFailure(c.w, c.format, failure)
	c.attempt.markClientByte(failure.Status)
}

// readFailure classifies an error while reading the provider's body.
func (c *call) readFailure(err error) Failure {
	if errors.Is(context.Cause(c.upstreamCtx), errIdleTimeout) ||
		errors.Is(c.upstreamCtx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return Failure{http.StatusGatewayTimeout, CodeProviderTimeout, ClassTimeout, "provider " + c.provider + " timed out"}
	}
	return invalidUpstream("provider " + c.provider + " ended the response early").failure
}

func hasMediaType(header http.Header, want string) bool {
	mediaType, _, err := mime.ParseMediaType(header.Get("Content-Type"))
	return err == nil && mediaType == want
}

// serveUpstreamError answers a non-success status. The body is not read at all, so
// nothing a provider echoes can reach the client, the log or the result.
func (c *call) serveUpstreamError(response *http.Response) {
	failure := upstreamStatusFailure(response.StatusCode, c.provider)
	retryAfter := ""
	if failure.Code == CodeProviderRateLimited || failure.Code == CodeProviderOverloaded {
		if value := response.Header.Get("Retry-After"); validRetryAfter(value) {
			retryAfter = value
		}
	}
	if failure.Code != CodeProviderRedirect && failure.Code != CodeInvalidUpstreamResponse {
		copyResponseHeaders(c.w.Header(), response.Header)
	}
	c.failBeforeCommit(failure, retryAfter)
}

// serveNormal buffers the response, up to a limit, because a limit breach or invalid
// JSON must still be reported with a proper status before any byte is sent.
func (c *call) serveNormal(response *http.Response) {
	limited := io.LimitReader(&firstByteReader{Reader: response.Body, mark: c.attempt.markUpstreamByte}, maxResponseBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		if c.clientCtx.Err() != nil {
			c.attempt.clientGone()
			return
		}
		c.failBeforeCommit(c.readFailure(err), "")
		return
	}
	if int64(len(body)) > maxResponseBytes {
		c.failBeforeCommit(invalidUpstream("provider "+c.provider+" response exceeds the size limit").failure, "")
		return
	}
	if !hasMediaType(response.Header, "application/json") || !json.Valid(body) {
		c.failBeforeCommit(invalidUpstream("provider "+c.provider+" sent an invalid response").failure, "")
		return
	}
	usage, counted, err := extractResponseUsage(c.format, c.envelope.Operation, body)
	if err != nil {
		c.failBeforeCommit(invalidUpstream("provider "+c.provider+" sent invalid usage").failure, "")
		return
	}
	result := &c.attempt.result
	result.Usage, result.CountedInputTokens, result.TerminalSeen = usage, counted, true
	result.UsageComplete = c.envelope.Operation == OperationGeneration && usage.primaryKnown()

	if rejection := c.handler.hooks.responseBody(c.upstreamCtx, c.envelope, body); rejection != nil {
		c.failBeforeCommit(rejection.failure(), "")
		return
	}

	copyResponseHeaders(c.w.Header(), response.Header)
	c.w.Header().Set("Content-Type", response.Header.Get("Content-Type"))
	c.w.prepareWrite(c.writeLimit)
	c.w.WriteHeader(response.StatusCode)
	c.attempt.markClientByte(response.StatusCode)
	if _, err := c.w.Write(body); err != nil {
		c.attempt.clientGone()
	}
}

// firstByteReader notes when the first byte of a body arrives.
type firstByteReader struct {
	io.Reader
	mark func()
}

func (r *firstByteReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if n > 0 {
		r.mark()
	}
	return n, err
}

// serveStream relays events as they complete. The status is committed only with the
// first event that reaches the client, so a failure before that still gets a real
// HTTP error instead of an error event.
func (c *call) serveStream(response *http.Response) {
	if !hasMediaType(response.Header, "text/event-stream") {
		c.failBeforeCommit(invalidUpstream("provider "+c.provider+" did not send an event stream").failure, "")
		return
	}
	idle := c.writeLimit
	timer := time.AfterFunc(idle, func() { c.cancelCause(errIdleTimeout) })
	defer timer.Stop()

	state := &streamState{format: c.format, askedUsage: c.envelope.ClientAskedUsage}
	defer func() {
		result := &c.attempt.result
		result.Usage, result.TerminalSeen, result.UsageComplete = state.usage, state.terminal, state.usageComplete()
	}()
	parser := newSSEParser()
	buffer := make([]byte, sseReadBuffer)
	header := response.Header

	for {
		n, readErr := response.Body.Read(buffer)
		if n > 0 {
			timer.Reset(idle)
			c.attempt.markUpstreamByte()
			parser.Feed(buffer[:n])
			if !c.relayEvents(parser, state, header) {
				return
			}
			if state.terminal {
				c.drainAfterTerminal(response.Body, timer)
				return
			}
		}
		if readErr == nil {
			continue
		}
		if event, ok := parser.Finish(); ok && readErr == io.EOF {
			if !c.relayEvent(event, state, header) {
				return
			}
			if state.terminal {
				return
			}
		}
		if c.clientCtx.Err() != nil {
			c.attempt.clientGone()
			return
		}
		failure := c.readFailure(readErr)
		if readErr == io.EOF {
			failure = invalidUpstream("provider " + c.provider + " ended the stream early").failure
		}
		c.endStream(failure)
		return
	}
}

// relayEvents returns false when the stream must stop.
func (c *call) relayEvents(parser *sseParser, state *streamState, header http.Header) bool {
	for {
		event, ok, err := parser.Next()
		if err != nil {
			c.endStream(invalidUpstream("provider " + c.provider + " sent an event over the size limit").failure)
			return false
		}
		if !ok {
			return true
		}
		if !c.relayEvent(event, state, header) {
			return false
		}
	}
}

func (c *call) relayEvent(event sseEvent, state *streamState, header http.Header) bool {
	action, failure := state.inspect(event, c.provider)
	if failure != nil {
		c.endStream(failure.failure)
		return false
	}
	if action == actionDrop {
		return true
	}
	if rejection := c.handler.hooks.streamEvent(c.upstreamCtx, c.envelope, event.Raw); rejection != nil {
		c.endStream(rejection.failure())
		return false
	}
	return c.writeEvent(event.Raw, header)
}

func (c *call) writeEvent(raw []byte, header http.Header) bool {
	if !c.w.committed {
		copyResponseHeaders(c.w.Header(), header)
		c.w.Header().Set("Content-Type", header.Get("Content-Type"))
		c.w.Header().Set("Cache-Control", "no-cache")
		c.w.Header().Set("X-Accel-Buffering", "no")
		c.w.prepareWrite(c.writeLimit)
		c.w.WriteHeader(http.StatusOK)
		c.attempt.markClientByte(http.StatusOK)
	}
	c.w.prepareWrite(c.writeLimit)
	if _, err := c.w.Write(raw); err != nil {
		c.attempt.clientGone()
		c.cancelCause(context.Canceled)
		return false
	}
	if err := c.w.flush(); err != nil {
		c.attempt.clientGone()
		c.cancelCause(context.Canceled)
		return false
	}
	return true
}

// endStream reports a failure in the only way still possible once committed: an
// error event followed by closing the stream.
func (c *call) endStream(failure Failure) {
	if !c.w.committed {
		c.failBeforeCommit(failure, "")
		return
	}
	c.attempt.fail(failure)
	c.w.prepareWrite(c.writeLimit)
	if _, err := c.w.Write(sseErrorEvent(c.format, failure)); err != nil {
		c.attempt.result.ClientDisconnected = true
		return
	}
	if c.w.flush() != nil {
		c.attempt.result.ClientDisconnected = true
	}
}

// drainAfterTerminal gives the provider a moment to end the body, which lets its
// connection be reused, without making the client wait for a silent provider.
func (c *call) drainAfterTerminal(body io.Reader, timer *time.Timer) {
	timer.Reset(terminalDrainWait)
	_, _ = io.CopyN(io.Discard, body, terminalDrainMax)
}
