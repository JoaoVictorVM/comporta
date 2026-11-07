package proxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/auth"
	"github.com/JoaoVictorVM/comporta/internal/config"
)

const (
	dialTimeout         = 10 * time.Second
	tlsHandshakeTimeout = 10 * time.Second
)

// errAddressRefused reports a destination that the address rules forbid.
var errAddressRefused = errors.New("destination address refused")

// Resolver looks up the addresses of a host.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// allowedAddress decides by provider category. Remote providers may never resolve
// into the local network: without this, a DNS answer could point a configured HTTPS
// provider at an internal service or the cloud metadata address.
func allowedAddress(addr netip.Addr, host string, local bool) bool {
	addr = addr.Unmap()
	if strings.EqualFold(host, "localhost") {
		return addr.IsLoopback()
	}
	if local {
		return addr.IsLoopback() || addr.IsPrivate()
	}
	return addr.IsGlobalUnicast() && !addr.IsPrivate() && !addr.IsLoopback() && !addr.IsLinkLocalUnicast()
}

type guardedDialer struct {
	local    bool
	resolver Resolver
	dialer   net.Dialer
}

// DialContext validates every resolved address before dialing and then connects to
// a validated IP, so a second lookup cannot return something different. TLS still
// uses the configured name, because the transport sets it from the URL.
func (d *guardedDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, errAddressRefused
	}
	var addrs []netip.Addr
	if literal, parseErr := netip.ParseAddr(host); parseErr == nil {
		addrs = []netip.Addr{literal}
	} else {
		addrs, err = d.resolver.LookupNetIP(ctx, "ip", host)
		if err != nil {
			return nil, err
		}
	}
	if len(addrs) == 0 {
		return nil, errAddressRefused
	}
	for _, addr := range addrs {
		if !allowedAddress(addr, host, d.local) {
			return nil, errAddressRefused
		}
	}
	var last error
	for _, addr := range addrs {
		conn, dialErr := d.dialer.DialContext(ctx, network, net.JoinHostPort(addr.Unmap().String(), port))
		if dialErr == nil {
			return conn, nil
		}
		last = dialErr
	}
	return nil, last
}

// transportKey separates transports by generation and provider: a reload can change
// the TLS floor or the header timeout, and operations in flight keep theirs.
type transportKey struct {
	generation int64
	provider   string
}

type poolEntry struct {
	transport *http.Transport
	refs      int
}

// TransportPool reuses provider connections. A generation is retired when a newer
// one appears: its idle connections close at once and its transports are released
// when its last operation ends. Provider secrets live in the snapshot and in each
// request's headers, never here.
type TransportPool struct {
	resolver Resolver
	rootCAs  *x509.CertPool

	mu      sync.Mutex
	entries map[transportKey]*poolEntry
	latest  int64
}

// NewTransportPool returns a pool. rootCAs is nil outside tests, which selects the
// system roots.
func NewTransportPool(resolver Resolver, rootCAs *x509.CertPool) *TransportPool {
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	return &TransportPool{resolver: resolver, rootCAs: rootCAs, entries: make(map[transportKey]*poolEntry)}
}

func (p *TransportPool) acquire(snapshot *config.Snapshot, name string, provider config.Provider) (*http.Transport, func()) {
	key := transportKey{snapshot.Generation(), name}
	p.mu.Lock()
	defer p.mu.Unlock()
	if key.generation > p.latest {
		p.latest = key.generation
		p.retireLocked()
	}
	entry, ok := p.entries[key]
	if !ok {
		entry = &poolEntry{transport: p.newTransport(snapshot, provider)}
		p.entries[key] = entry
	}
	entry.refs++
	var once sync.Once
	return entry.transport, func() {
		once.Do(func() { p.release(key, entry) })
	}
}

func (p *TransportPool) release(key transportKey, entry *poolEntry) {
	p.mu.Lock()
	defer p.mu.Unlock()
	entry.refs--
	if entry.refs == 0 && key.generation < p.latest {
		entry.transport.CloseIdleConnections()
		delete(p.entries, key)
	}
}

func (p *TransportPool) retireLocked() {
	for key, entry := range p.entries {
		if key.generation >= p.latest {
			continue
		}
		entry.transport.CloseIdleConnections()
		if entry.refs == 0 {
			delete(p.entries, key)
		}
	}
}

// Close drops every idle connection, for shutdown.
func (p *TransportPool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, entry := range p.entries {
		entry.transport.CloseIdleConnections()
	}
}

func (p *TransportPool) newTransport(snapshot *config.Snapshot, provider config.Provider) *http.Transport {
	minVersion := uint16(tls.VersionTLS12)
	if provider.TLSMinVersion == "1.3" {
		minVersion = tls.VersionTLS13
	}
	dialer := &guardedDialer{local: provider.Local, resolver: p.resolver, dialer: net.Dialer{Timeout: dialTimeout}}
	return &http.Transport{
		// Environment proxies would send prompts and provider keys through a host
		// the configuration never named.
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		TLSClientConfig:       &tls.Config{MinVersion: minVersion, RootCAs: p.rootCAs},
		TLSHandshakeTimeout:   tlsHandshakeTimeout,
		ResponseHeaderTimeout: snapshot.Listeners().UpstreamHeaderTimeout,
		DisableCompression:    true,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
	}
}

// upstreamURL joins the configured base with an authorized path. One trailing /v1
// is optional on the base, because both spellings are common for the same API.
func upstreamURL(base string, route Route) (*url.URL, error) {
	parsed, err := url.Parse(base)
	if err != nil {
		return nil, err
	}
	prefix := strings.TrimSuffix(parsed.EscapedPath(), "/")
	prefix = strings.TrimSuffix(prefix, "/v1")
	return parsed.Parse(prefix + route.path())
}

var forwardedHeaders = map[auth.Format][]string{
	auth.FormatAnthropic: {"Content-Type", "Anthropic-Version", "Anthropic-Beta"},
	auth.FormatOpenAI:    {"Content-Type"},
}

// connectionNamed lists headers the client declared hop-by-hop.
func connectionNamed(header http.Header) map[string]bool {
	named := map[string]bool{}
	for _, value := range header.Values("Connection") {
		for token := range strings.SplitSeq(value, ",") {
			named[http.CanonicalHeaderKey(strings.TrimSpace(token))] = true
		}
	}
	return named
}

// newUpstreamRequest builds the outgoing request from nothing, instead of filtering
// a copy, so that a header added by a future SDK can never leak by default.
func newUpstreamRequest(ctx context.Context, target *url.URL, incoming http.Header, format auth.Format, secret config.Secret, hasSecret, stream bool, body []byte) (*http.Request, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), io.NopCloser(bytes.NewReader(body)))
	if err != nil {
		return nil, err
	}
	// Without these the transport would be free to replay a POST on a reused
	// connection, and one explicit call must be at most one attempt.
	request.GetBody = nil
	request.ContentLength = int64(len(body))

	named := connectionNamed(incoming)
	for _, name := range forwardedHeaders[format] {
		if named[name] {
			continue
		}
		if values := incoming.Values(name); len(values) > 0 {
			request.Header[name] = append([]string(nil), values...)
		}
	}
	if hasSecret {
		if format == auth.FormatAnthropic {
			request.Header.Set("X-Api-Key", secret.Reveal())
		} else {
			request.Header.Set("Authorization", "Bearer "+secret.Reveal())
		}
	}
	accept := "application/json"
	if stream {
		accept = "text/event-stream"
	}
	request.Header.Set("Accept", accept)
	request.Header.Set("Accept-Encoding", "identity")
	request.Header.Set("User-Agent", "comporta")
	return request, nil
}

// classifyTransportError turns a failed round trip into a public failure. connected
// tells whether a connection was obtained, which separates "could not reach it" from
// "it went silent", without parsing error strings.
func classifyTransportError(err error, connected bool, provider string) Failure {
	var timeout net.Error
	timedOut := errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &timeout) && timeout.Timeout())
	if !connected {
		message := "provider " + provider + " unreachable"
		if timedOut {
			message += " (connect timeout after 10s)"
		}
		return Failure{http.StatusBadGateway, CodeProviderUnreachable, ClassConnection, message}
	}
	if timedOut {
		return Failure{http.StatusGatewayTimeout, CodeProviderTimeout, ClassTimeout, "provider " + provider + " timed out"}
	}
	return Failure{http.StatusBadGateway, CodeProviderUnreachable, ClassConnection, "provider " + provider + " unreachable"}
}
