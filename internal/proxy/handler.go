// Package proxy implements the request pipeline between SDK clients and providers.
package proxy

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync/atomic"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/auth"
	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/runtimecontrol"
	"github.com/JoaoVictorVM/comporta/internal/useridentity"
)

// Rejection stops a request at a pipeline stage with a sanitized answer.
type Rejection struct {
	Status  int
	Code    Code
	Class   ErrorClass
	Message string
	// RetryAfter is in whole seconds and only set when an honest value exists.
	RetryAfter int64
	Limit      *LimitDetail
}

func (r *Rejection) failure() Failure {
	return Failure{Status: r.Status, Code: r.Code, Class: r.Class, Message: r.Message}
}

// RequestContent gives a stage private access to the request during the operation.
// It must not be retained after the stage returns.
type RequestContent struct {
	Document *Document
	Body     []byte
}

// Hooks are the fixed points where later stages plug into the pipeline, in the order
// they run: Prepare, BeforeSend, then ResponseBody or StreamEvent for what comes back.
// Results go to the ResultSink. A nil hook does nothing.
type Hooks struct {
	Prepare      func(ctx context.Context, envelope *Envelope, content RequestContent) *Rejection
	BeforeSend   func(ctx context.Context, envelope *Envelope, content RequestContent) *Rejection
	ResponseBody func(ctx context.Context, envelope *Envelope, body []byte) *Rejection
	StreamEvent  func(ctx context.Context, envelope *Envelope, raw []byte) *Rejection
}

func (h Hooks) responseBody(ctx context.Context, envelope *Envelope, body []byte) *Rejection {
	if h.ResponseBody == nil {
		return nil
	}
	return h.ResponseBody(ctx, envelope, body)
}

func (h Hooks) streamEvent(ctx context.Context, envelope *Envelope, raw []byte) *Rejection {
	if h.StreamEvent == nil {
		return nil
	}
	return h.StreamEvent(ctx, envelope, raw)
}

// Gate admits a request against the operator overrides and returns the ticket
// that must be released once its completion reached accounting.
type Gate interface {
	Admit(runtimecontrol.Subject) (*runtimecontrol.Ticket, error)
}

// Options configures a Handler.
type Options struct {
	Accounting Accounting
	// Gate applies pauses and deletion barriers. Without it nothing is paused.
	Gate       Gate
	Auth       *auth.HTTP
	Transports *TransportPool
	Sink       ResultSink
	Hooks      Hooks
	// Loop cuts runaway agents after BeforeSend and before accounting. Without
	// it no loop is detected.
	Loop *LoopStage
	// Identity pseudonymizes end users. Without it conversation requests fail: leaving
	// it out never turns pseudonymization off.
	Identity *useridentity.Service
	Logger   *slog.Logger
	// Now replaces the clock in tests.
	Now func() time.Time
}

// Handler serves the fixed routes.
type Handler struct {
	accounting Accounting
	gate       Gate
	auth       *auth.HTTP
	transports *TransportPool
	sink       ResultSink
	hooks      Hooks
	loop       *LoopStage
	identity   *useridentity.Service
	logger     *slog.Logger
	now        func() time.Time
}

// NewHandler returns a Handler.
func NewHandler(options Options) *Handler {
	handler := &Handler{
		accounting: options.Accounting, gate: options.Gate,
		auth: options.Auth, transports: options.Transports, sink: options.Sink,
		hooks: options.Hooks, loop: options.Loop, identity: options.Identity, logger: options.Logger, now: options.Now,
	}
	if handler.transports == nil {
		handler.transports = NewTransportPool(nil, nil)
	}
	if handler.sink == nil {
		handler.sink = discardSink{}
	}
	if handler.logger == nil {
		handler.logger = slog.New(slog.DiscardHandler)
	}
	if handler.now == nil {
		handler.now = time.Now
	}
	return handler
}

// Close releases idle provider connections.
func (h *Handler) Close() { h.transports.Close() }

// trackedWriter records whether the response was committed, which decides how a
// later failure can still be reported.
type trackedWriter struct {
	http.ResponseWriter
	controller *http.ResponseController
	committed  bool
}

func newTrackedWriter(w http.ResponseWriter) *trackedWriter {
	return &trackedWriter{ResponseWriter: w, controller: http.NewResponseController(w)}
}

func (w *trackedWriter) WriteHeader(status int) {
	w.committed = true
	w.ResponseWriter.WriteHeader(status)
}

func (w *trackedWriter) Write(data []byte) (int, error) {
	w.committed = true
	return w.ResponseWriter.Write(data)
}

// Unwrap lets ResponseController reach the underlying connection.
func (w *trackedWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *trackedWriter) flush() error {
	if err := w.controller.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	return nil
}

// prepareWrite renews the write deadline, so a client that stops reading cannot hold
// the goroutine, while a long stream with a live client is never cut.
func (w *trackedWriter) prepareWrite(limit time.Duration) {
	_ = w.controller.SetWriteDeadline(time.Now().Add(limit))
}

// ServeRoute authenticates and serves one request using the snapshot the caller
// captured, so that the whole operation sees one configuration generation.
func (h *Handler) ServeRoute(w http.ResponseWriter, r *http.Request, route Route, snapshot *config.Snapshot) {
	format := route.format(r)
	tracked := newTrackedWriter(w)
	op := &operation{handler: h, w: tracked, raw: w, format: format, route: route, snapshot: snapshot, admission: h.now()}
	defer op.finish()

	authenticated, ok := h.auth.Authorize(tracked, r, format, snapshot)
	if !ok {
		return
	}
	identity, ok := auth.IdentityFrom(authenticated.Context())
	if !ok {
		WriteFailure(tracked, format, internalFailure())
		return
	}
	_, rawQuery, hasQuery := strings.Cut(r.RequestURI, "?")
	if route == RouteModels {
		serveModels(tracked, authenticated, format, snapshot, identity.AppName(), rawQuery)
		return
	}
	if hasQuery {
		WriteFailure(tracked, format, invalidRequest("this endpoint accepts no query parameters"))
		return
	}
	op.serve(authenticated)
}

// operation is one authenticated conversation request.
type operation struct {
	completion func(AttemptResult)
	handler    *Handler
	w          *trackedWriter
	raw        http.ResponseWriter
	format     auth.Format
	route      Route
	snapshot   *config.Snapshot
	admission  time.Time
	attempt    *attempt
	ticket     *runtimecontrol.Ticket
	stream     bool
}

// finish publishes the attempt and keeps a panic to this request. It is the one
// place that ends an attempt, so a panic is recorded on the result before it goes
// out. The panic value may hold request content, so neither it nor the stack is
// logged.
func (o *operation) finish() {
	value := recover()
	abort := value == http.ErrAbortHandler
	if value != nil && !abort {
		abort = o.answerPanic()
	}
	// Admission is already durable. Release a buffered JSON answer before the
	// completion write, whose failure remains recoverable from that journal.
	if value == nil && o.handler.accounting != nil && !o.stream && o.w.committed && o.attempt != nil {
		if err := o.w.flush(); err != nil {
			o.attempt.clientGone()
		}
	}
	if o.attempt != nil {
		o.attempt.finish()
	}
	// The completion is in the accounting queue by now, which is what a deletion
	// barrier waits for.
	o.ticket.Release()
	if abort {
		panic(http.ErrAbortHandler)
	}
}

// answerPanic reports whether the only remaining answer is to abort the connection.
func (o *operation) answerPanic() bool {
	o.handler.logger.Error("request handler panicked")
	failure := internalFailure()
	if o.attempt != nil {
		o.attempt.fail(failure)
	}
	switch {
	case !o.w.committed:
		WriteFailure(o.w, o.format, failure)
		if o.attempt != nil {
			o.attempt.markClientByte(failure.Status)
		}
	case o.stream:
		_, _ = o.w.Write(sseErrorEvent(o.format, failure))
		_ = o.w.flush()
	default:
		// A JSON response cannot take an error after it started: closing the
		// connection is what tells the client that the body is incomplete.
		return true
	}
	return false
}

func (o *operation) serve(r *http.Request) {
	listeners := o.snapshot.Listeners()
	if failure, ok := checkRequestHeaders(r); !ok {
		WriteFailure(o.w, o.format, failure)
		return
	}
	body, failure := o.readBody(r, listeners)
	if failure != nil {
		if failure.Code != CodeClientDisconnected {
			WriteFailure(o.w, o.format, *failure)
		}
		return
	}
	identity, _ := auth.IdentityFrom(r.Context())
	request, failure := parseRequest(o.route, r, body, identity, o.snapshot.Generation())
	if failure != nil {
		WriteFailure(o.w, o.format, *failure)
		return
	}
	envelope := request.envelope
	o.stream = envelope.Stream
	if failure := request.applyIdentity(o.handler.identity, o.route, o.snapshot); failure != nil {
		WriteFailure(o.w, o.format, *failure)
		return
	}
	if o.handler.gate != nil {
		ticket, err := o.handler.gate.Admit(runtimecontrol.Subject{App: envelope.AppName, Hash: envelope.Identity.Hash().Hex(), Anonymous: envelope.Identity.Anonymous()})
		if err != nil {
			WriteFailure(o.w, o.format, gateFailure(err, envelope.AppName))
			return
		}
		o.ticket = ticket
	}

	hooks := o.handler.hooks
	content := RequestContent{Document: request.document, Body: body}
	if hooks.Prepare != nil {
		if rejection := hooks.Prepare(r.Context(), envelope, content); rejection != nil {
			WriteFailure(o.w, o.format, rejection.failure())
			return
		}
	}

	providerName, provider, failure := o.resolveProvider(envelope.ModelEffective)
	if failure != nil {
		WriteFailure(o.w, o.format, *failure)
		return
	}

	if o.handler.accounting != nil {
		app, _ := o.snapshot.App(envelope.AppName)
		if err := request.capOutput(app.MaxTokens); err != nil {
			WriteFailure(o.w, o.format, internalFailure())
			return
		}
	}
	effective := request.effectiveBody()
	if hooks.BeforeSend != nil {
		content.Body = effective
		if rejection := hooks.BeforeSend(r.Context(), envelope, content); rejection != nil {
			WriteFailure(o.w, o.format, rejection.failure())
			return
		}
	}
	// Counted once per HTTP request, before any reservation: a cut never
	// reserves budget and never reaches the provider.
	if rejection := o.handler.loop.evaluate(r.Context(), o.snapshot, envelope); rejection != nil {
		writeRejection(o.w, o.format, rejection)
		return
	}
	if o.handler.accounting != nil {
		admission, rejection := o.handler.accounting.Admit(r.Context(), o.snapshot, envelope, providerName, provider)
		if rejection != nil {
			writeRejection(o.w, o.format, rejection)
			return
		}
		o.completion = admission.Complete
		applyBudgetHeaders(o.w.Header(), admission.Budget)
	}
	o.send(r, envelope, providerName, provider, effective, listeners)
}

// readBody applies the size limit and the same total deadline as the rest of the
// operation, so a client sending slowly cannot hold a request past it.
func (o *operation) readBody(r *http.Request, listeners config.Listeners) ([]byte, *Failure) {
	limit := listeners.MaxBodyBytes
	if r.ContentLength > limit {
		failure := tooLarge(limit)
		return nil, &failure
	}
	_ = o.w.controller.SetReadDeadline(o.admission.Add(listeners.NonStreamingTimeout))
	body, err := io.ReadAll(http.MaxBytesReader(o.raw, r.Body, limit))
	// From here on, reads only watch for the client leaving, which must not expire.
	_ = o.w.controller.SetReadDeadline(time.Time{})
	if err == nil {
		return body, nil
	}
	// The rest of the body will never be read. Without this, net/http would try to
	// consume it before answering and wait on the same stalled client.
	o.w.Header().Set("Connection", "close")
	var tooBig *http.MaxBytesError
	var timeout net.Error
	var failure Failure
	switch {
	case errors.As(err, &tooBig):
		failure = tooLarge(limit)
	case errors.As(err, &timeout) && timeout.Timeout():
		failure = Failure{http.StatusGatewayTimeout, CodeProviderTimeout, ClassTimeout, "request deadline exceeded while reading the body"}
	case r.Context().Err() != nil:
		failure = Failure{Code: CodeClientDisconnected, Class: ClassClientCancel}
	default:
		failure = invalidRequest("request body could not be read")
	}
	return nil, &failure
}

// resolveProvider finds the destination from the configuration alone. Nothing the
// client sends other than the model name influences it.
func (o *operation) resolveProvider(model string) (string, config.Provider, *Failure) {
	name, ok := o.snapshot.ProviderForModel(model)
	if !ok {
		failure := Failure{http.StatusBadRequest, CodeModelUnknown, ClassInvalidRequest, "model is not declared by any provider"}
		return "", config.Provider{}, &failure
	}
	provider, ok := o.snapshot.Provider(name)
	if !ok {
		failure := internalFailure()
		return "", config.Provider{}, &failure
	}
	if provider.APIFormat != providerFormat(o.format) {
		failure := Failure{http.StatusBadRequest, CodeProviderFormatMismatch, ClassInvalidRequest,
			"model belongs to a provider with a different API format than this endpoint"}
		return "", config.Provider{}, &failure
	}
	return name, provider, nil
}

func (o *operation) send(r *http.Request, envelope *Envelope, name string, provider config.Provider, body []byte, listeners config.Listeners) {
	handler := o.handler
	o.attempt = newAttempt(envelope, name, o.admission, handler.now, handler.sink, handler.logger)
	o.attempt.complete = o.completion
	o.attempt.result.NotSent = true

	ctx := r.Context()
	var cancelDeadline context.CancelFunc = func() {}
	idle := listeners.StreamingIdleTimeout
	if !envelope.Stream {
		ctx, cancelDeadline = context.WithDeadline(ctx, o.admission.Add(listeners.NonStreamingTimeout))
		idle = time.Until(o.admission.Add(listeners.NonStreamingTimeout))
	}
	defer cancelDeadline()
	upstreamCtx, cancelCause := context.WithCancelCause(ctx)
	defer cancelCause(nil)

	current := &call{
		handler: handler, w: o.w, format: o.format, envelope: envelope, attempt: o.attempt,
		provider: name, clientCtx: r.Context(), upstreamCtx: upstreamCtx, cancelCause: cancelCause, writeLimit: idle,
	}

	target, err := upstreamURL(provider.BaseURL, o.route)
	if err != nil {
		current.failBeforeCommit(internalFailure(), "")
		return
	}
	secret, hasSecret := o.snapshot.ProviderAPIKey(name)
	var connected atomic.Bool
	traced := httptrace.WithClientTrace(upstreamCtx, &httptrace.ClientTrace{
		GotConn: func(httptrace.GotConnInfo) { connected.Store(true) },
	})
	request, err := newUpstreamRequest(traced, target, r.Header, o.format, secret, hasSecret, envelope.Stream, body)
	if err != nil {
		current.failBeforeCommit(internalFailure(), "")
		return
	}
	transport, release := handler.transports.acquire(o.snapshot, name, provider)
	defer release()

	o.attempt.result.NotSent = false
	response, err := transport.RoundTrip(request)
	if err != nil {
		if r.Context().Err() != nil {
			o.attempt.clientGone()
			return
		}
		current.failBeforeCommit(classifyTransportError(err, connected.Load(), name), "")
		return
	}
	defer func() { _ = response.Body.Close() }()
	o.attempt.markUpstreamHeaders(response.StatusCode)

	switch {
	case response.StatusCode < 200 || response.StatusCode >= 300:
		current.serveUpstreamError(response)
	case envelope.Stream:
		current.serveStream(response)
	default:
		current.serveNormal(response)
	}
	if !current.attempt.result.Committed && r.Context().Err() != nil {
		o.attempt.clientGone()
	}
}
