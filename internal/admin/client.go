package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

var (
	// ErrUnreachable means no request reached the gateway.
	ErrUnreachable = errors.New("admin API unreachable")
	// ErrOutcomeUnknown means the request was sent but its answer was lost: the
	// operation may or may not have been applied.
	ErrOutcomeUnknown = errors.New("admin API outcome unknown")
	// ErrBadURL rejects an admin URL that could leak or misroute the token.
	ErrBadURL = errors.New("admin URL must be http://host:port for a loopback address or https://host[:port], without user, path, query or fragment")
)

// APIError is a failure answered by the admin API.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string { return e.Message }

// Client calls the admin API of a running gateway. The token goes only in the
// Authorization header; redirects are never followed and no proxy from the
// environment is used.
type Client struct {
	base  *url.URL
	token string
	http  *http.Client
}

// NewClient validates the destination. Plain HTTP is accepted only for a
// loopback address; anything remote needs HTTPS with standard verification.
func NewClient(raw, token string, timeout time.Duration) (*Client, error) {
	base, err := url.Parse(raw)
	if err != nil || base.User != nil || base.RawQuery != "" || base.ForceQuery || base.Fragment != "" || base.Opaque != "" || (base.Path != "" && base.Path != "/") || base.Host == "" {
		return nil, ErrBadURL
	}
	switch base.Scheme {
	case "https":
	case "http":
		if !loopback(base.Hostname()) {
			return nil, ErrBadURL
		}
	default:
		return nil, ErrBadURL
	}
	base.Path = ""
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &Client{base: base, token: token, http: &http.Client{
		Transport: transport, Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	addr, err := netip.ParseAddr(host)
	return err == nil && addr.IsLoopback()
}

// Target is the host and port shown in messages; it never carries the token.
func (c *Client) Target() string { return c.base.Host }

// Do sends one call. A nil in skips the body; a nil out discards the answer.
func (c *Client) Do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	var wrote atomic.Bool
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{WroteRequest: func(httptrace.WroteRequestInfo) { wrote.Store(true) }})
	request, err := http.NewRequestWithContext(ctx, method, c.base.String()+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	response, err := c.http.Do(request)
	if err != nil {
		if wrote.Load() {
			return ErrOutcomeUnknown
		}
		return ErrUnreachable
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(response.Body, 16<<20))
	if err != nil {
		return ErrOutcomeUnknown
	}
	if response.StatusCode >= 300 {
		var envelope errorEnvelope
		if json.Unmarshal(data, &envelope) != nil || envelope.Error.Code == "" {
			return &APIError{Status: response.StatusCode, Code: "unexpected_response", Message: fmt.Sprintf("unexpected answer from the admin API (HTTP %d)", response.StatusCode)}
		}
		return &APIError{Status: response.StatusCode, Code: envelope.Error.Code, Message: envelope.Error.Message}
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return ErrOutcomeUnknown
	}
	return nil
}

// LocalURL turns a listener bind address into the URL a local client uses: a
// wildcard bind is reached through the loopback address.
func LocalURL(bind string, port int64) string {
	host := bind
	switch strings.Trim(bind, "[]") {
	case "", "0.0.0.0", "::":
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(strings.Trim(host, "[]"), fmt.Sprint(port))
}
