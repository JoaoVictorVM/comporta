package alerts

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/storage"
)

// RetryDelays are the minimum waits after the first, second and third failed
// calls. A delivery makes at most four calls.
var RetryDelays = [...]time.Duration{2 * time.Second, 8 * time.Second, 32 * time.Second}

// Closed categories of an attempt. Nothing else about a receiver's answer is
// ever stored or logged.
const (
	CategorySuccess        = "success"
	CategoryTimeout        = "timeout"
	CategoryNetwork        = "network"
	CategoryRateLimited    = "rate_limited"
	CategoryServerError    = "server_error"
	CategoryClientError    = "client_error"
	CategoryInvalid        = "invalid_response"
	CategoryChannelChanged = "channel_changed"
)

// pool runs deliveries with a process-wide ceiling. Jobs get a context of their
// own, which a shutdown cancels only after the grace period.
type pool struct {
	slots  chan struct{}
	wg     sync.WaitGroup
	ctx    context.Context
	cancel context.CancelFunc
	freed  chan struct{}
}

func newPool(n int) *pool {
	ctx, cancel := context.WithCancel(context.Background())
	return &pool{slots: make(chan struct{}, n), ctx: ctx, cancel: cancel, freed: make(chan struct{}, 1)}
}

func (p *pool) free() int { return cap(p.slots) - len(p.slots) }

// idle wakes the worker when a job ends, so the channel's next delivery is
// claimed without waiting for the poll.
func (p *pool) idle() <-chan struct{} { return p.freed }

func (p *pool) run(job func(context.Context)) {
	p.slots <- struct{}{}
	p.wg.Go(func() {
		defer func() {
			<-p.slots
			select {
			case p.freed <- struct{}{}:
			default:
			}
		}()
		job(p.ctx)
	})
}

// shutdown lets running calls finish within the grace period, then cancels
// them; a canceled job returns its claim to pending.
func (p *pool) shutdown(after func(time.Duration) <-chan time.Time, grace time.Duration) {
	done := make(chan struct{})
	go func() { p.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-after(grace):
		p.cancel()
		<-done
	}
	p.cancel()
}

// deliverDue claims as many due deliveries as there are free slots, at most
// one per channel, and starts them.
func (s *Service) deliverDue(ctx context.Context, p *pool) int {
	free := min(p.free(), storage.MaxAlertBatch)
	if ctx.Err() != nil || free == 0 {
		return 0
	}
	claimed, err := s.o.Store.ClaimAlertDeliveries(ctx, s.o.Now(), ClaimLease, free)
	if err != nil {
		if ctx.Err() == nil {
			s.storeFailed("claim_deliveries", err)
		}
		return 0
	}
	for _, d := range claimed {
		p.run(func(job context.Context) { s.deliver(job, d) })
	}
	return len(claimed)
}

// DeliverOnce claims and sends due deliveries synchronously. Tests use it to
// step the lifecycle without the worker loop.
func (s *Service) DeliverOnce(ctx context.Context) int {
	p := newPool(MaxConcurrent)
	n := s.deliverDue(ctx, p)
	p.shutdown(s.o.After, ShutdownGrace)
	return n
}

func (s *Service) deliver(ctx context.Context, d storage.AlertDelivery) {
	result := s.attempt(ctx, d)
	// A shutdown interrupted the call: the claim goes back to pending and the
	// attempt is not counted, since its outcome is unknown.
	if ctx.Err() != nil && result.Outcome != storage.AlertDelivered {
		release, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.o.Store.ReleaseAlertDelivery(release, d.ID, d.ClaimToken)
		return
	}
	finish, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := s.o.Store.FinishAlertDelivery(finish, d.ID, d.ClaimToken, result, s.o.Now())
	switch {
	case errors.Is(err, storage.ErrClaimLost):
		s.logger.Warn("alert delivery claim expired before the result was saved", "channel", d.Channel, "type", d.ChannelType, "delivery_id", d.ID)
		return
	case err != nil:
		s.storeFailed("finish_delivery", err)
		return
	}
	s.metrics.AlertDelivery(d.ChannelType, result.Outcome, result.Error)
	switch result.Outcome {
	case storage.AlertFailed:
		s.logger.Warn("alert delivery failed", "channel", d.Channel, "type", d.ChannelType, "category", result.Error, "status", result.Status, "attempts", d.Attempts+1)
	case storage.AlertCanceled:
		s.logger.Info("alert delivery canceled; the channel was removed or changed", "channel", d.Channel, "type", d.ChannelType)
	case storage.AlertRetry:
		if d.Attempts+1 >= storage.MaxAlertAttempts {
			s.logger.Warn("alert delivery failed", "channel", d.Channel, "type", d.ChannelType, "category", result.Error, "status", result.Status, "attempts", d.Attempts+1)
		}
	}
}

// attempt resolves the channel from the current snapshot and makes one call.
// A channel removed, renamed to another type or without its secret is
// canceled: a pending delivery never goes to a different destination.
func (s *Service) attempt(ctx context.Context, d storage.AlertDelivery) storage.AlertResult {
	canceled := storage.AlertResult{Outcome: storage.AlertCanceled, Error: CategoryChannelChanged}
	snapshot := s.o.Source.Capture()
	channel, ok := snapshot.Alerts().Channels[d.Channel]
	if !ok || string(channel.Type) != d.ChannelType {
		return canceled
	}
	payload, err := DecodePayload(d.Occurrence.Payload)
	if err != nil {
		return storage.AlertResult{Outcome: storage.AlertFailed, Error: CategoryInvalid}
	}
	out, ok := s.outbound(snapshot, d, channel, payload)
	if !ok {
		return canceled
	}
	r, err := s.call(ctx, out)
	return s.classify(d, r, err)
}

func zonesOf(snapshot *config.Snapshot) Zones {
	return func(app string) string {
		if a, ok := snapshot.App(app); ok && a.Timezone != "" {
			return a.Timezone
		}
		if tz := snapshot.Timezone(); tz != "" {
			return tz
		}
		return "UTC"
	}
}

func (s *Service) outbound(snapshot *config.Snapshot, d storage.AlertDelivery, channel config.AlertChannel, p Payload) (outbound, bool) {
	zones := zonesOf(snapshot)
	switch channel.Type {
	case config.AlertWebhook:
		target, ok := snapshot.AlertSecret(d.Channel, "url")
		secret, signed := snapshot.AlertSecret(d.Channel, "signing_secret")
		if !ok || !signed {
			return outbound{}, false
		}
		timestamp := strconv.FormatInt(s.o.Now().Unix(), 10)
		body := d.Occurrence.Payload
		return outbound{url: target.Reveal(), body: body, headers: map[string]string{
			"X-Comporta-Timestamp":   timestamp,
			"X-Comporta-Signature":   Sign([]byte(secret.Reveal()), timestamp, body),
			"X-Comporta-Delivery-ID": d.ID,
		}}, true
	case config.AlertDiscord:
		target, ok := snapshot.AlertSecret(d.Channel, "url")
		if !ok {
			return outbound{}, false
		}
		confirmed, err := withWait(target.Reveal())
		if err != nil {
			return outbound{}, false
		}
		return outbound{url: confirmed, body: DiscordBody(p, zones)}, true
	case config.AlertSlack:
		target, ok := snapshot.AlertSecret(d.Channel, "url")
		if !ok {
			return outbound{}, false
		}
		return outbound{url: target.Reveal(), body: SlackBody(p, zones)}, true
	case config.AlertNtfy:
		if channel.ServerURL == "" || channel.Topic == "" {
			return outbound{}, false
		}
		out := outbound{url: channel.ServerURL, body: NtfyBody(p, zones, channel.Topic), headers: map[string]string{}}
		if token, ok := snapshot.AlertSecret(d.Channel, "token"); ok {
			out.headers["Authorization"] = "Bearer " + token.Reveal()
		}
		return out, true
	}
	return outbound{}, false
}

// classify reduces an answer to a closed category. Timeouts, connection
// errors, 408, 425, 429 and 5xx are transient; every other answer, a redirect
// included, is permanent.
func (s *Service) classify(d storage.AlertDelivery, r reply, err error) storage.AlertResult {
	retry := func(category string, status int, hint time.Duration) storage.AlertResult {
		delay := RetryDelays[min(d.Attempts, len(RetryDelays)-1)]
		return storage.AlertResult{Outcome: storage.AlertRetry, Error: category, Status: status, RetryAt: s.o.Now().Add(max(delay, hint)), Sent: true}
	}
	failed := func(category string, status int) storage.AlertResult {
		return storage.AlertResult{Outcome: storage.AlertFailed, Error: category, Status: status, Sent: true}
	}
	if err != nil {
		var netErr net.Error
		if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
			return retry(CategoryTimeout, 0, 0)
		}
		return retry(CategoryNetwork, 0, 0)
	}
	switch {
	case r.status == 429:
		return retry(CategoryRateLimited, r.status, r.retryAfter)
	case r.status == 408:
		return retry(CategoryTimeout, r.status, 0)
	case r.status == 425:
		return retry(CategoryRateLimited, r.status, r.retryAfter)
	case r.status >= 500:
		return retry(CategoryServerError, r.status, 0)
	case r.status >= 300 && r.status < 400:
		return failed(CategoryInvalid, r.status)
	case r.status >= 400:
		return failed(CategoryClientError, r.status)
	case r.status < 200:
		return failed(CategoryInvalid, r.status)
	}
	if !confirmed(d.ChannelType, r) {
		return failed(CategoryInvalid, r.status)
	}
	return storage.AlertResult{Outcome: storage.AlertDelivered, Error: CategorySuccess, Status: r.status, Sent: true}
}

// confirmed applies each destination's proof of creation: any 2xx for a
// generic webhook; 200 with the created message for Discord; 200 with "ok"
// for Slack; 2xx with the publish record for ntfy.
func confirmed(channelType string, r reply) bool {
	switch channelType {
	case "webhook":
		return true
	case "discord":
		return r.status == 200 && hasID(r.body)
	case "slack":
		return r.status == 200 && strings.TrimSuffix(strings.TrimSuffix(string(r.body), "\n"), "\r") == "ok"
	case "ntfy":
		return hasID(r.body)
	}
	return false
}

func hasID(body []byte) bool {
	var message struct {
		ID json.RawMessage `json:"id"`
	}
	return json.Unmarshal(body, &message) == nil && len(message.ID) > 0 && string(message.ID) != "null"
}
