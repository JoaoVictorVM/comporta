# Alerts

Comporta turns spend and protection events into alerts and delivers each one to every configured channel: a generic signed webhook, Discord and Slack incoming webhooks, and ntfy, public or self-hosted. Alerts never take part in a request: evaluation and delivery run in a background worker, and a receiver that is slow, down or wrong never delays, blocks or fails a request.

## Settings

```yaml
alerts:
  thresholds: [50, 80, 100]      # budget percentages, 1 to 100, strictly increasing
  anomaly_multiplier: 5          # positive integer
  anomaly_min_usd: 0.50          # minimum spend in the last hour
  channels:
    ops-webhook:
      type: webhook
      url: ${ALERT_WEBHOOK_URL}              # or url_file
      signing_secret_file: /run/secrets/alert-signing   # or signing_secret
    ops-discord:
      type: discord
      url: ${DISCORD_WEBHOOK_URL}
    ops-slack:
      type: slack
      url_file: /run/secrets/slack-webhook
    ops-ntfy:
      type: ntfy
      server_url: https://ntfy.example.com
      topic: comporta-alerts                 # 1 to 64 letters, digits, _ or -
      token: ${NTFY_TOKEN}                   # optional
```

Webhook, Discord and Slack URLs, signing secrets and ntfy tokens are secrets: they come from the environment or from files, never from the YAML, and they are never stored, logged or shown. Every URL must be absolute `http` or `https` without credentials or fragment. Plain `http` is accepted only for `localhost`, loopback or private IP literals, which is how a self-hosted receiver on the same network is reached. The ntfy `server_url` is not a secret.

Deliveries use a dedicated client: it never follows a redirect, ignores `HTTP_PROXY`, `HTTPS_PROXY` and `NO_PROXY`, waits at most 5 seconds for the headers and 10 seconds in total, and reads at most 4 KiB of an answer, which it then discards.

## Triggers

| Trigger | When | Deduplication key |
|---|---|---|
| `budget_threshold` | The confirmed spend of an app budget, or of one user's per-user budget, reaches a configured threshold. Every threshold not yet sent below the current use is sent, so a jump from 49% to 101% sends 50, 80 and 100 | App, scope, user, dimension, window start and threshold |
| `hard_cap` | A request is refused by a hard cap | App, scope, user, dimension and window |
| `anomaly` | Every 5 minutes, for each app: the last hour (the twelve closed 5 minute buckets) is at least `anomaly_min_usd` and greater than `anomaly_multiplier` times the hourly average of the 7 days before it, hours without spend included. Equal is not greater. With no history, reaching the minimum is enough | App and UTC quarter hour |
| `loop_cut` | The loop guard cuts a user's requests. The first cut of the quarter hour is sent; later ones are absorbed | App and UTC quarter hour |
| `fallback_burst` | A fallback makes 5 or more for the app in the moving 5 minutes that end with it | App and UTC quarter hour |
| `config_reload_failed` | A changed configuration file is rejected; the previous configuration stays in force | UTC quarter hour |
| `fail_open` | Limit state cannot be verified for an app whose fail mode is `fail_open`. Apps in `fail_closed` refuse requests instead and are not alerted | App and UTC quarter hour |

Budgets use confirmed spend, the settled cost of finished requests, never reservations of requests still running. The windows are the budget windows, in the budget's timezone. Startup and every applied reload evaluate the current spend of every app again, so a lowered budget or a new threshold alerts without waiting for traffic; an alert already sent is not repeated. Historical events from before alerts were enabled are not replayed.

## Payload of the generic webhook

`POST` with `Content-Type: application/json` and `User-Agent: comporta-alerts/<version>`:

```json
{
  "id": "7fd3c61d21d14a7da7f61683a5b617c2",
  "type": "budget_threshold",
  "occurred_at": "2026-11-23T18:30:00Z",
  "app": "bot-discord",
  "scope": "app",
  "dimension": "usd_per_day",
  "unit": "microusd",
  "used": 1600000,
  "limit": 2000000,
  "threshold_percent": 80,
  "window_start": "2026-11-23T03:00:00Z",
  "window_end": "2026-11-24T03:00:00Z",
  "renews_at": "2026-11-24T03:00:00Z"
}
```

| Field | Meaning |
|---|---|
| `id` | The alert: 32 hex digits, the same for every channel |
| `type` | The trigger |
| `occurred_at` | When the fact happened or was evaluated, RFC 3339 UTC |
| `app` | The configured app name; absent for `config_reload_failed` |
| `scope` | `global`, `app` or `user` |
| `user_hash` | The user's pseudonym, 32 hex digits, only for user budgets, user hard caps and loop cuts |
| `dimension`, `unit` | The budget dimension and its unit, `microusd` or `tokens` |
| `used`, `limit` | Confirmed spend and ceiling, in the unit |
| `threshold_percent` | The threshold reached |
| `window_start`, `window_end`, `renews_at` | The window of the budget, the loop or the anomaly |
| `count` | Repeats of a loop, fallbacks of a burst, or the number of problems of a rejected reload |
| `multiplier`, `baseline_microusd`, `last_hour_microusd` | The anomaly rule and its two amounts |
| `generation` | The configuration generation that stays active after a rejected reload |

Absent fields are omitted. New fields may be added; receivers should ignore the ones they do not know.

### Verifying a delivery

Each delivery carries:

- `X-Comporta-Timestamp`: Unix seconds of the attempt.
- `X-Comporta-Signature`: `sha256=` followed by the lowercase hex HMAC-SHA256, keyed with the signing secret, of the timestamp, a dot and the exact body bytes.
- `X-Comporta-Delivery-ID`: the same for every attempt of this alert on this channel.

```python
import hashlib, hmac, time

def verify(secret: bytes, timestamp: str, signature: str, body: bytes) -> bool:
    if abs(time.time() - int(timestamp)) > 300:
        return False
    expected = "sha256=" + hmac.new(secret, timestamp.encode() + b"." + body, hashlib.sha256).hexdigest()
    return hmac.compare_digest(expected, signature)
```

Verify before parsing, reject timestamps more than 5 minutes away, and deduplicate on the delivery ID: a network failure after the receiver accepted a call makes Comporta try again, so the same delivery can arrive twice. Any 2xx confirms the delivery; the body of the answer is not read.

## Discord, Slack and ntfy

Messages come from fixed templates: names, amounts, times and categories only, at most 2,000 characters for Discord and 3,000 for Slack and ntfy. User pseudonyms appear abbreviated, such as `u_9f2c4f…`.

- **Discord**: the message is sent with `wait=true` added to the webhook URL, and only HTTP 200 with the created message confirms it. Mentions are disabled.
- **Slack**: a plain `text` message, without Block Kit; only HTTP 200 with the body `ok` confirms it.
- **ntfy**: a JSON publish to `server_url` with the topic as a field, title `Comporta alert`, priority 4 (5 for hard caps, budgets at 100% and `fail_open`) and the tag `warning`; the token, when set, goes as `Authorization: Bearer`. Email, calls, attachments, actions, click URLs and delays are never requested.

## Retries and failures

A call that times out, fails to connect or answers 408, 425, 429 or 5xx is retried three times, after at least 2, 8 and 32 seconds; a `Retry-After` longer than that is honored up to 5 minutes. Any other answer, a redirect or an answer that does not confirm the message fails at once: a wrong URL or token does not generate more calls. A delivery makes at most four calls.

A failure is logged with the channel name, its type, a closed category (`timeout`, `network`, `rate_limited`, `server_error`, `client_error` or `invalid_response`) and the HTTP status, never the URL, the token, the body or the answer.

Each channel has at most one delivery in flight, which keeps the order of its messages and never bursts a receiver; a process runs at most eight deliveries at a time.

## Durability, restarts and reloads

Every relevant event enters an evaluation queue in the same transaction that stores it, and each alert and each delivery are rows of the usage store. The event bus only wakes the worker; when a notification is lost, the next poll, within a second, finds the work. Consequently:

- A restart resumes pending evaluations and retries where they stopped. An alert already confirmed on a channel is never sent to it again.
- A claim on an evaluation or a delivery expires after 30 seconds, so the work of a process that died is taken over. On a clean shutdown, calls in flight get up to 10 seconds and whatever is unfinished goes back to the queue.
- A channel is identified by its name. Removing it, or changing its type, cancels its pending deliveries; nothing is ever sent to another destination. A changed URL or token is used from the next attempt on. Renaming a channel creates another one. Adding a channel does not resend alerts already created.
- A removed threshold does not cancel an alert already created; a new one can alert on the next evaluation.
- Evaluation and delivery capture the configuration once; a reload in between restarts the evaluation, so no alert is expanded for a set of channels that no longer exists.
- At most 100,000 deliveries can be open. At that ceiling, evaluation waits and nothing is dropped; metering and limits continue.

Finished evaluations are kept 7 days, finished alerts and deliveries 30 days (an alert of a budget window still open is kept until the window ends) and the 5 minute cost aggregate 8 days.

## Multiple instances

In a distributed deployment every replica evaluates and delivers from the shared Postgres store; Redis is not used for alerts. Claims carry a lease and a token, and unique keys deduplicate: two replicas that evaluate the same event, or the same anomaly, create one alert and one delivery per channel. A replica that dies while delivering leaves its claim to expire, and another one sends the same delivery again with the same delivery ID.

## Operation

The admin listener's `/metrics` exposes:

| Metric | Labels | Meaning |
|---|---|---|
| `comporta_alert_occurrences_total` | `trigger` | Alerts created by this process |
| `comporta_alert_deliveries_total` | `channel_type`, `outcome`, `category` | Delivery attempts finished by this process |
| `comporta_alert_store_failures_total` | | Store operations of the worker that failed and will be retried |
| `comporta_alert_inputs_pending` | | Events waiting for evaluation |
| `comporta_alert_deliveries_pending` | | Deliveries not yet confirmed, failed or canceled |
| `comporta_alert_delivery_oldest_seconds` | | Age of the oldest open delivery |

In a cluster, aggregate the gauges with `max`. A growing `comporta_alert_delivery_oldest_seconds` means a receiver keeps failing or the store is unavailable. The panel's overview lists the 10 most recent alerts.

## Limits

- There is no email, SMS, PagerDuty or Alertmanager channel, no configurable template, no manual resend and no delivery history in the panel.
- Delivery is at least once to a receiver: a network failure after a receiver accepted a call can produce a duplicate, which the delivery ID lets the receiver discard.
- Budget alerts follow settled spend, so they can arrive after the request that crossed the threshold finished, never before.
