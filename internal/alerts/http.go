package alerts

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"net/url"
	"runtime/debug"
	"strconv"
	"time"
)

// Transport limits. The total timeout fits inside the claim lease, so a slow
// receiver never outlives the claim of the delivery it serves.
const (
	RequestTimeout   = 10 * time.Second
	HeaderTimeout    = 5 * time.Second
	MaxResponseBytes = 4 << 10
	MaxRetryAfter    = 5 * time.Minute
)

// NewClient builds the only client deliveries use. It never follows a
// redirect, which could carry a secret URL elsewhere, and ignores the proxy
// variables of the environment.
func NewClient() *http.Client {
	dialer := &net.Dialer{Timeout: HeaderTimeout, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:   HeaderTimeout,
		ResponseHeaderTimeout: HeaderTimeout,
		MaxIdleConns:          16,
		MaxIdleConnsPerHost:   2,
		MaxConnsPerHost:       2,
		IdleConnTimeout:       90 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	return &http.Client{
		Transport:     transport,
		Timeout:       RequestTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// Sign is the generic webhook signature: HMAC-SHA256 over the ASCII
// timestamp, a dot and the exact body bytes.
func Sign(secret []byte, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(timestamp))
	mac.Write([]byte{'.'})
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// Verify is what a receiver does: constant time comparison and a five minute
// tolerance on the timestamp.
func Verify(secret []byte, timestamp, signature string, body []byte, now time.Time) bool {
	seconds, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return false
	}
	skew := now.Sub(time.Unix(seconds, 0))
	if skew > 5*time.Minute || skew < -5*time.Minute {
		return false
	}
	return hmac.Equal([]byte(Sign(secret, timestamp, body)), []byte(signature))
}

// outbound is one call to a receiver.
type outbound struct {
	url     string
	body    []byte
	headers map[string]string
}

// reply is the bounded result of a call. The body is read up to
// MaxResponseBytes and never leaves the classification step.
type reply struct {
	status     int
	body       []byte
	retryAfter time.Duration
}

func (s *Service) call(ctx context.Context, out outbound) (reply, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, out.url, bytes.NewReader(out.body))
	if err != nil {
		return reply{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "comporta-alerts/"+s.o.Version)
	for k, v := range out.headers {
		req.Header.Set(k, v)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return reply{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes))
	return reply{status: resp.StatusCode, body: body, retryAfter: retryAfter(resp.Header.Get("Retry-After"), s.o.Now())}, nil
}

// retryAfter accepts seconds or an HTTP date and is bounded to five minutes.
func retryAfter(value string, now time.Time) time.Duration {
	if value == "" {
		return 0
	}
	var d time.Duration
	if seconds, err := strconv.ParseFloat(value, 64); err == nil {
		if seconds <= 0 {
			return 0
		}
		d = time.Duration(seconds * float64(time.Second))
	} else if at, err := http.ParseTime(value); err == nil {
		d = at.Sub(now)
	}
	return min(max(d, 0), MaxRetryAfter)
}

// withWait adds wait=true to a Discord URL without touching anything else in
// it, so the reply confirms the message was created.
func withWait(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	query := u.Query()
	if query.Get("wait") == "true" {
		return raw, nil
	}
	if u.RawQuery == "" || !query.Has("wait") {
		if u.RawQuery != "" {
			u.RawQuery += "&"
		}
		u.RawQuery += "wait=true"
		return u.String(), nil
	}
	query.Set("wait", "true")
	u.RawQuery = query.Encode()
	return u.String(), nil
}

func buildVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}
