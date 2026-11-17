package proxy

import (
	"crypto/sha256"
	"encoding/binary"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/auth"
	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/responsecache"
	"github.com/JoaoVictorVM/comporta/internal/telemetry"
)

const (
	cacheHeader = "x-comporta-cache"
	cacheHit    = "hit"
)

// cacheState is the cache side of one operation: the key of the request as
// admitted, and the capture of its first attempt once a response starts.
type cacheState struct {
	cache      *responsecache.Cache
	key        responsecache.Key
	generation int64
	ttl        time.Duration
	stream     bool
	builder    *responsecache.Builder
}

// cacheFor returns the cache state of a request that may read and feed the
// cache, or nil. It runs after admission, on the candidate the policy chose,
// so a hit never escapes authentication, pauses, the loop guard or limits.
func (o *operation) cacheFor(r *http.Request, app config.App, choice chosen) *cacheState {
	cache := o.handler.cache
	e := choice.envelope
	if cache == nil || o.handler.accounting == nil || !app.Cache.Enabled || choice.request == nil || choice.request.uncacheable ||
		e.Operation != OperationGeneration || !e.Temperature.Known || !zeroNumber(e.Temperature.Value) {
		return nil
	}
	parts := responsecache.KeyParts{
		Generation: o.snapshot.Generation(), App: e.AppName, Format: strconv.Itoa(int(o.format)), Endpoint: e.Endpoint,
		Provider: choice.providerName, Model: e.ModelEffective, Stream: e.Stream,
		HeaderDigest: semanticHeaderDigest(r.Header, o.format), BodyDigest: e.RequestHash,
	}
	if app.Cache.SharedAcrossUsers {
		parts.Namespace, parts.BodyDigest = responsecache.Shared, choice.request.sharedRequestHash()
	} else {
		parts.Namespace, parts.Anonymous = e.Identity.Hash().Hex(), e.Identity.Anonymous()
	}
	return &cacheState{cache: cache, key: responsecache.NewKey(parts), generation: o.snapshot.Generation(), ttl: time.Duration(app.Cache.TTLSeconds) * time.Second, stream: e.Stream}
}

// zeroNumber reports whether a JSON number lexeme is mathematically zero. It
// reads the digits instead of the parsed value, because a tiny non-zero value
// can underflow to 0 when parsed.
func zeroNumber(lexeme string) bool {
	mantissa, _, _ := strings.Cut(strings.ToLower(strings.TrimPrefix(lexeme, "-")), "e")
	digits := strings.Replace(mantissa, ".", "", 1)
	return digits != "" && strings.Trim(digits, "0") == ""
}

// semanticHeaderDigest covers the headers forwarded to the provider that may
// change the answer, such as the protocol version. Credentials and tracing
// headers are never forwarded, so they never enter it.
func semanticHeaderDigest(header http.Header, format auth.Format) [32]byte {
	h := sha256.New()
	named := connectionNamed(header)
	var size [8]byte
	write := func(s string) {
		binary.BigEndian.PutUint64(size[:], uint64(len(s)))
		h.Write(size[:])
		h.Write([]byte(s))
	}
	for _, name := range forwardedHeaders[format] {
		if named[name] {
			continue
		}
		values := header.Values(name)
		write(name)
		write(strconv.Itoa(len(values)))
		for _, value := range values {
			write(value)
		}
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// lookup returns a usable lease, or nil. An entry whose shape does not match
// the request is evicted before anything is sent.
func (s *cacheState) lookup() *responsecache.Lease {
	lease, ok := s.cache.Lookup(s.key, s.generation)
	if !ok {
		return nil
	}
	valid := lease.Kind() == responsecache.KindJSON && !s.stream && len(lease.Body()) > 0
	if s.stream {
		valid = lease.Kind() == responsecache.KindSSE && len(lease.Events()) > 0
	}
	if !valid {
		lease.Release()
		s.cache.Invalidate(s.key)
		return nil
	}
	return lease
}

// capture copies one piece of what was delivered. The first piece starts the
// capture; once abandoned it never restarts.
func (s *cacheState) capture(kind responsecache.Kind, contentType string, data []byte) {
	if s == nil {
		return
	}
	if s.builder == nil {
		s.builder = s.cache.Begin(s.key, s.generation, kind, contentType)
		if s.builder == nil {
			s.cache = nil
			return
		}
	}
	s.builder.Append(data)
}

// abort discards the capture. Safe on nil and after publication.
func (s *cacheState) abort() {
	if s != nil {
		s.builder.Abort()
		s.builder = nil
	}
}

// publish stores the capture only when the first attempt answered with a
// complete, successful response that reached the client and whose result was
// made durable. Anything else discards it.
func (o *operation) publishCapture(panicked bool) {
	s := o.cache
	if s == nil || s.builder == nil {
		return
	}
	defer s.abort()
	if panicked || o.attempt == nil || !o.concluded || o.concludeErr != nil || o.terminal != nil {
		return
	}
	result := o.attempt.result
	if result.CacheHit || result.NotSent || result.AttemptIndex != 1 || result.ErrorCode != "" || result.ClientDisconnected ||
		!result.Committed || !result.TerminalSeen || !result.UsageComplete || result.DownstreamStatus != known(http.StatusOK) {
		return
	}
	// A reload may have landed while the response was produced. Seeing it here
	// makes the capture of the old generation fail instead of publishing.
	if source := o.handler.source; source != nil {
		s.cache.Observe(source.Capture().Generation())
	}
	s.builder.Publish(s.ttl)
}

// replay answers from a stored response. Only the current request's headers
// go out with it: nothing of the provider's original exchange is reused. The
// result costs nothing; a failure while writing never reaches a provider.
func (o *operation) replay(r *http.Request, choice chosen, lease *responsecache.Lease, listeners config.Listeners) {
	defer lease.Release()
	handler := o.handler
	o.attempt = newAttempt(choice.envelope, choice.providerName, o.admission, handler.now, handler.sink, handler.logger)
	o.attempt.complete = o.completion
	o.attempt.result.NotSent = true
	o.attempt.result.CacheHit = true
	o.attempt.tel = o.tel
	o.cacheHit = true
	o.tel.CacheHit()
	deliver := o.tel.Stage(telemetry.StageDeliver)
	defer deliver.End()

	header := o.w.Header()
	header.Set(cacheHeader, cacheHit)
	header.Set("Content-Type", lease.ContentType())
	if lease.Kind() == responsecache.KindJSON {
		body := lease.Body()
		header.Set("Content-Length", strconv.Itoa(len(body)))
		limit := max(time.Until(o.admission.Add(listeners.NonStreamingTimeout)), time.Second)
		o.w.prepareWrite(limit)
		o.w.WriteHeader(http.StatusOK)
		o.attempt.markClientByte(http.StatusOK)
		if _, err := o.w.Write(body); err != nil {
			o.attempt.clientGone()
		}
		return
	}
	header.Set("Cache-Control", "no-cache")
	header.Set("X-Accel-Buffering", "no")
	limit := listeners.StreamingIdleTimeout
	o.w.prepareWrite(limit)
	o.w.WriteHeader(http.StatusOK)
	o.attempt.markClientByte(http.StatusOK)
	for _, event := range lease.Events() {
		if r.Context().Err() != nil {
			o.attempt.clientGone()
			return
		}
		o.w.prepareWrite(limit)
		if _, err := o.w.Write(event); err != nil {
			o.attempt.clientGone()
			return
		}
		if err := o.w.flush(); err != nil {
			o.attempt.clientGone()
			return
		}
	}
}
