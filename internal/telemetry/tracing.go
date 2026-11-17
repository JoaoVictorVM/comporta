package telemetry

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
	"go.opentelemetry.io/otel/trace"
)

// SemconvVersion is the revision of the OpenTelemetry semantic conventions,
// GenAI included, that the span attributes follow. Upgrading the dependency
// never renames these attributes on its own.
const SemconvVersion = "1.39.0"

const (
	tracerName = "github.com/JoaoVictorVM/comporta"
	rootName   = "comporta.request"
	attemptOp  = "upstream_attempt"
	tracesPath = "/v1/traces"
)

// Attribute keys outside the semantic conventions belong to Comporta.
const (
	keyRequestStream = attribute.Key("gen_ai.request.stream")
	keyApp           = attribute.Key("comporta.app")
	keyOperation     = attribute.Key("comporta.operation")
	keyAttempt       = attribute.Key("comporta.attempt")
	keyOutcome       = attribute.Key("comporta.outcome")
	keyCost          = attribute.Key("comporta.cost_microusd")
	keyCacheHit      = attribute.Key("comporta.cache_hit")
	keyLimitType     = attribute.Key("comporta.limit.type")
	keyLimitScope    = attribute.Key("comporta.limit.scope")
)

// AllowedAttributes is the complete set of span attribute keys Comporta
// writes. Content, identities and free text never have a key here.
var AllowedAttributes = map[attribute.Key]bool{
	semconv.GenAIOperationNameKey: true, semconv.GenAIProviderNameKey: true, semconv.GenAIRequestModelKey: true,
	semconv.GenAIResponseModelKey: true, semconv.GenAIUsageInputTokensKey: true, semconv.GenAIUsageOutputTokensKey: true,
	keyRequestStream: true, semconv.ErrorTypeKey: true, semconv.HTTPResponseStatusCodeKey: true,
	keyApp: true, keyOperation: true, keyAttempt: true, keyOutcome: true, keyCost: true, keyCacheHit: true,
	keyLimitType: true, keyLimitScope: true,
}

func allowed(attrs []attribute.KeyValue) []attribute.KeyValue {
	out := attrs[:0]
	for _, kv := range attrs {
		if AllowedAttributes[kv.Key] {
			out = append(out, kv)
		}
	}
	return out
}

func attrApp(app string) attribute.KeyValue { return keyApp.String(app) }

// traceProvider is the tracer of one telemetry generation. At most two exist:
// the current one and one draining the sessions that began before a reload.
type traceProvider struct {
	endpoint string
	tp       *sdktrace.TracerProvider
	tracer   trace.Tracer
	queue    *queueProcessor
	sessions int
	draining bool
	retired  atomic.Bool
	// lost is the drop reason of spans its sessions end after retirement.
	lost     string
	stopping atomic.Bool
}

// traceSession is the trace of one request, bound to the provider in force
// when it began.
type traceSession struct {
	provider *traceProvider
	ctx      context.Context
	root     trace.Span
	genCtx   context.Context
	gen      trace.Span
}

func (t *traceSession) live() bool { return t != nil && !t.provider.retired.Load() }

// retire stops a provider's sessions from producing spans. The caller holds
// the service lock.
func (p *traceProvider) retire(reason string) {
	if !p.retired.Load() {
		p.lost = reason
		p.retired.Store(true)
	}
}

func (s *Service) openSession(op *Op) *traceSession {
	s.mu.Lock()
	provider := s.current
	if provider == nil || s.closed {
		s.mu.Unlock()
		return nil
	}
	provider.sessions++
	s.mu.Unlock()
	// A new root every time: no remote parent, baggage or trace state is read
	// from the client.
	ctx, root := provider.tracer.Start(context.Background(), rootName,
		trace.WithNewRoot(), trace.WithSpanKind(trace.SpanKindServer), trace.WithTimestamp(op.start),
		trace.WithAttributes(allowed([]attribute.KeyValue{keyOperation.String(string(op.operation)), attrApp(unauthenticated)})...))
	return &traceSession{provider: provider, ctx: ctx, root: root}
}

func (s *Service) closeSession(t *traceSession) {
	if t == nil {
		return
	}
	s.mu.Lock()
	p := t.provider
	p.sessions--
	idle := p.draining && p.sessions == 0
	if idle {
		s.removeDraining(p)
	}
	s.mu.Unlock()
	if idle {
		s.shutdownProvider(p, DropShutdownTimeout)
	}
}

func (t *traceSession) rootAttrs(attrs ...attribute.KeyValue) {
	if t.live() {
		t.root.SetAttributes(allowed(attrs)...)
	}
}

func (t *traceSession) stage(stage Stage, attempt int) trace.Span {
	if !t.live() {
		return nil
	}
	var attrs []attribute.KeyValue
	if attempt > 0 {
		attrs = append(attrs, keyAttempt.Int(attempt))
	}
	_, span := t.provider.tracer.Start(t.ctx, string(stage), trace.WithSpanKind(trace.SpanKindInternal), trace.WithAttributes(allowed(attrs)...))
	return span
}

// attempt opens the span of one upstream attempt under the logical GenAI
// operation, which is created by the first attempt of a generation.
func (t *traceSession) attempt(o *Op, a *Attempt) trace.Span {
	if !t.live() {
		return nil
	}
	parent := t.ctx
	o.mu.Lock()
	requested, stream := o.requested, o.stream
	o.mu.Unlock()
	if o.operation == OperationGeneration {
		if t.gen == nil {
			t.genCtx, t.gen = t.provider.tracer.Start(t.ctx, "chat "+requested, trace.WithSpanKind(trace.SpanKindClient), trace.WithTimestamp(a.started),
				trace.WithAttributes(allowed([]attribute.KeyValue{
					semconv.GenAIOperationNameKey.String("chat"), semconv.GenAIRequestModelKey.String(requested),
					keyRequestStream.Bool(stream),
				})...))
		}
		parent = t.genCtx
	}
	attrs := []attribute.KeyValue{keyAttempt.Int(a.index), semconv.GenAIRequestModelKey.String(a.model), semconv.GenAIProviderNameKey.String(a.provider)}
	_, span := t.provider.tracer.Start(parent, attemptOp, trace.WithSpanKind(trace.SpanKindInternal), trace.WithTimestamp(a.started), trace.WithAttributes(allowed(attrs)...))
	return span
}

func (t *traceSession) finishAttempt(a *Attempt, end AttemptEnd, input Optional[int64], cost Optional[int64], ended time.Time) {
	if a.span == nil {
		return
	}
	// A retired provider no longer hands spans to its processor.
	if !t.live() {
		a.op.s.dropSpans(t.provider.lost, 1)
		return
	}
	attrs := []attribute.KeyValue{keyOutcome.String(string(end.Result))}
	if input.Known {
		attrs = append(attrs, semconv.GenAIUsageInputTokensKey.Int64(input.Value))
	}
	if end.OutputTokens.Known {
		attrs = append(attrs, semconv.GenAIUsageOutputTokensKey.Int64(end.OutputTokens.Value))
	}
	if cost.Known {
		attrs = append(attrs, keyCost.Int64(cost.Value))
	}
	if end.Status.Known {
		attrs = append(attrs, semconv.HTTPResponseStatusCodeKey.Int(end.Status.Value))
	}
	failed := end.Result != AttemptOK
	if failed {
		attrs = append(attrs, semconv.ErrorTypeKey.String(errorTypeOr(end.ErrorType, string(end.Result))))
	}
	a.span.SetAttributes(allowed(attrs)...)
	spanStatus(a.span, failed)
	a.span.End(trace.WithTimestamp(ended))
}

func errorTypeOr(value, fallback string) string {
	if v := errorType(value); v != "" {
		return v
	}
	return fallback
}

// end closes the GenAI operation and the root. Every attribute is a closed
// category or a configured name.
func (t *traceSession) end(o *Op, end RequestEnd, outcome Outcome) {
	if t == nil {
		return
	}
	if !t.live() {
		lost := 1
		if t.gen != nil {
			lost++
		}
		o.s.dropSpans(t.provider.lost, lost)
		return
	}
	o.mu.Lock()
	model, provider, cacheHit, cost, input, output, lastEnd := o.model, o.provider, o.cacheHit, o.cost, o.input, o.output, o.lastEnd
	o.mu.Unlock()
	failed := outcome == OutcomeError
	if t.gen != nil {
		attrs := []attribute.KeyValue{semconv.GenAIResponseModelKey.String(model), semconv.GenAIProviderNameKey.String(provider), keyOutcome.String(string(outcome))}
		if input.Known {
			attrs = append(attrs, semconv.GenAIUsageInputTokensKey.Int64(input.Value))
		}
		if output.Known {
			attrs = append(attrs, semconv.GenAIUsageOutputTokensKey.Int64(output.Value))
		}
		if cost.Known {
			attrs = append(attrs, keyCost.Int64(cost.Value))
		}
		if failed {
			attrs = append(attrs, semconv.ErrorTypeKey.String(errorTypeOr(end.ErrorType, "error")))
		}
		t.gen.SetAttributes(allowed(attrs)...)
		spanStatus(t.gen, failed)
		if lastEnd.IsZero() {
			lastEnd = o.s.now()
		}
		t.gen.End(trace.WithTimestamp(lastEnd))
	}
	attrs := []attribute.KeyValue{keyOutcome.String(string(outcome)), keyCacheHit.Bool(cacheHit)}
	if end.Status.Known {
		attrs = append(attrs, semconv.HTTPResponseStatusCodeKey.Int(end.Status.Value))
	}
	if blockTypes[end.Block.Type] && blockScopes[end.Block.Scope] {
		attrs = append(attrs, keyLimitType.String(string(end.Block.Type)), keyLimitScope.String(string(end.Block.Scope)))
	}
	if failed {
		attrs = append(attrs, semconv.ErrorTypeKey.String(errorTypeOr(end.ErrorType, "error")))
	}
	t.root.SetAttributes(allowed(attrs)...)
	spanStatus(t.root, failed)
	t.root.End()
}

// QueueOptions bounds the span queue of each provider.
type QueueOptions struct {
	Size          int
	Batch         int
	FlushInterval time.Duration
	ExportTimeout time.Duration
}

func (q QueueOptions) withDefaults() QueueOptions {
	if q.Size <= 0 {
		q.Size = 2048
	}
	if q.Batch <= 0 {
		q.Batch = 512
	}
	if q.FlushInterval <= 0 {
		q.FlushInterval = 5 * time.Second
	}
	if q.ExportTimeout <= 0 {
		q.ExportTimeout = 5 * time.Second
	}
	return q
}

// queueProcessor exports finished spans in batches from a bounded queue. The
// request path only ever tries to enqueue: a full queue drops the span and
// counts it, and nothing waits on the network or the disk.
type queueProcessor struct {
	exporter sdktrace.SpanExporter
	options  QueueOptions
	resource *resource.Resource
	dropped  func(reason string, n int)
	failed   func()

	queue chan sdktrace.ReadOnlySpan
	flush chan chan struct{}
	stop  chan struct{}
	done  chan struct{}

	mu         sync.RWMutex
	stopped    bool
	stopReason string
	deadline   context.Context
	once       sync.Once
	// abort ends an export still running when the shutdown deadline passes.
	abort       context.Context
	cancelAbort context.CancelFunc
}

func newQueueProcessor(exporter sdktrace.SpanExporter, options QueueOptions, res *resource.Resource, dropped func(string, int), failed func()) *queueProcessor {
	p := &queueProcessor{
		exporter: exporter, options: options, resource: res, dropped: dropped, failed: failed,
		queue: make(chan sdktrace.ReadOnlySpan, options.Size), flush: make(chan chan struct{}),
		stop: make(chan struct{}), done: make(chan struct{}), deadline: context.Background(),
	}
	p.abort, p.cancelAbort = context.WithCancel(context.Background())
	go p.run()
	return p
}

func (p *queueProcessor) OnStart(context.Context, sdktrace.ReadWriteSpan) {}

// fixedResource replaces whatever resource the SDK merged from the process
// environment with the two fixed attributes Comporta exports.
type fixedResource struct {
	sdktrace.ReadOnlySpan
	resource *resource.Resource
}

func (f fixedResource) Resource() *resource.Resource { return f.resource }

func (p *queueProcessor) OnEnd(span sdktrace.ReadOnlySpan) {
	if !span.SpanContext().IsSampled() {
		return
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.stopped {
		p.dropped(p.stopReason, 1)
		return
	}
	select {
	case p.queue <- fixedResource{span, p.resource}:
	default:
		p.dropped(DropQueueFull, 1)
	}
}

func (p *queueProcessor) run() {
	defer close(p.done)
	ticker := time.NewTicker(p.options.FlushInterval)
	defer ticker.Stop()
	batch := make([]sdktrace.ReadOnlySpan, 0, p.options.Batch)
	send := func() {
		if len(batch) > 0 {
			p.export(p.abort, batch)
			batch = batch[:0]
		}
	}
	for {
		select {
		case span := <-p.queue:
			batch = append(batch, span)
			if len(batch) >= p.options.Batch {
				send()
			}
		case <-ticker.C:
			send()
		case reply := <-p.flush:
			for drained := false; !drained; {
				select {
				case span := <-p.queue:
					batch = append(batch, span)
					if len(batch) >= p.options.Batch {
						send()
					}
				default:
					drained = true
				}
			}
			send()
			close(reply)
		case <-p.stop:
			p.drain(batch)
			return
		}
	}
}

// drain exports what is left within the shutdown deadline and counts the rest.
func (p *queueProcessor) drain(batch []sdktrace.ReadOnlySpan) {
	for {
		select {
		case span := <-p.queue:
			batch = append(batch, span)
			continue
		default:
		}
		break
	}
	for len(batch) > 0 {
		if p.deadline.Err() != nil {
			p.dropped(DropShutdownTimeout, len(batch))
			return
		}
		n := min(len(batch), p.options.Batch)
		p.export(p.deadline, batch[:n])
		batch = batch[n:]
	}
}

func (p *queueProcessor) export(parent context.Context, batch []sdktrace.ReadOnlySpan) {
	ctx, cancel := context.WithTimeout(parent, p.options.ExportTimeout)
	defer cancel()
	if err := p.exporter.ExportSpans(ctx, batch); err != nil {
		p.dropped(DropExportFailed, len(batch))
		p.failed()
	}
}

func (p *queueProcessor) ForceFlush(ctx context.Context) error {
	reply := make(chan struct{})
	select {
	case p.flush <- reply:
	case <-p.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-reply:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// halt stops accepting spans; the ones ending later are counted with reason.
func (p *queueProcessor) halt(reason string) {
	p.mu.Lock()
	if !p.stopped {
		p.stopped, p.stopReason = true, reason
	}
	p.mu.Unlock()
}

func (p *queueProcessor) Shutdown(ctx context.Context) error {
	var err error
	p.once.Do(func() {
		p.halt(DropShutdownTimeout)
		p.mu.Lock()
		p.deadline = ctx
		p.mu.Unlock()
		close(p.stop)
		select {
		case <-p.done:
		case <-ctx.Done():
			err = ctx.Err()
			// The export in flight is abandoned; what remains is counted.
			p.cancelAbort()
			select {
			case <-p.done:
			case <-time.After(100 * time.Millisecond):
			}
		}
		p.cancelAbort()
		if shutdownErr := p.exporter.Shutdown(ctx); err == nil {
			err = shutdownErr
		}
	})
	return err
}

// newResource holds only the service name and the binary version. No
// detector of host, process, user or environment runs.
func newResource(version string) *resource.Resource {
	return resource.NewSchemaless(semconv.ServiceName("comporta"), semconv.ServiceVersion(version))
}

func (s *Service) newProvider(endpoint string) (*traceProvider, error) {
	exporter, err := s.exporter(endpoint)
	if err != nil {
		return nil, err
	}
	res := newResource(s.version)
	queue := newQueueProcessor(exporter, s.queue, res, s.dropSpans, func() { s.diagnose(ComponentTracing, ReasonExportFailed) })
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSpanProcessor(queue),
		sdktrace.WithRawSpanLimits(sdktrace.SpanLimits{
			AttributeValueLengthLimit: 256, AttributeValueDepthLimit: 1, AttributeCountLimit: 32, EventCountLimit: 8,
			LinkCountLimit: 8, AttributePerEventCountLimit: 32, AttributePerLinkCountLimit: 32,
		}),
	)
	return &traceProvider{endpoint: endpoint, tp: tp, tracer: tp.Tracer(tracerName), queue: queue}, nil
}

var quietOnce sync.Once

// quietOTel keeps the SDK from printing raw exporter errors, which may hold
// URLs or response bodies. Failures are counted and logged by the service
// with fixed fields instead.
func quietOTel() {
	quietOnce.Do(func() {
		otel.SetErrorHandler(otel.ErrorHandlerFunc(func(error) {}))
		otel.SetLogger(logr.Discard())
	})
}

// TracesURL appends /v1/traces to the configured base URL unless the path
// already ends with it.
func TracesURL(base string) (string, error) {
	parsed, err := url.Parse(base)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", errors.New("invalid OpenTelemetry endpoint")
	}
	path := strings.TrimRight(parsed.Path, "/")
	if !strings.HasSuffix(path, tracesPath) {
		path += tracesPath
	}
	parsed.Path, parsed.RawPath = path, ""
	return parsed.String(), nil
}

var errPublicDestination = errors.New("plain http telemetry destination is not private")

// PrivateAddress reports whether an address may receive plain HTTP telemetry.
func PrivateAddress(addr netip.Addr) bool {
	addr = addr.Unmap()
	return addr.IsLoopback() || addr.IsPrivate()
}

// newOTLPExporter builds the OTLP/HTTP protobuf exporter. It never follows a
// redirect or an environment proxy, and plain HTTP only reaches addresses
// that resolve to loopback or private networks.
func newOTLPExporter(endpoint string) (sdktrace.SpanExporter, error) {
	target, err := TracesURL(endpoint)
	if err != nil {
		return nil, err
	}
	plain := strings.HasPrefix(target, "http://")
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	if plain {
		dialer.Control = func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			addr, err := netip.ParseAddr(host)
			if err != nil || !PrivateAddress(addr) {
				return errPublicDestination
			}
			return nil
		}
	}
	transport := &http.Transport{
		Proxy: nil, DialContext: dialer.DialContext, ForceAttemptHTTP2: true,
		MaxIdleConns: 4, IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 5 * time.Second,
	}
	client := &http.Client{
		Transport:     transport,
		Timeout:       5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return otlptracehttp.New(context.Background(),
		otlptracehttp.WithEndpointURL(target),
		otlptracehttp.WithHTTPClient(client),
		otlptracehttp.WithTimeout(5*time.Second),
		otlptracehttp.WithRetry(otlptracehttp.RetryConfig{Enabled: true, InitialInterval: 250 * time.Millisecond, MaxInterval: time.Second, MaxElapsedTime: 5 * time.Second}),
	)
}
