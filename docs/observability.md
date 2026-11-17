# Observability

Comporta exposes Prometheus metrics on the admin listener, can send one OpenTelemetry trace per proxy request over OTLP/HTTP, and writes structured JSON logs. A Compose profile starts Prometheus, Grafana with a provisioned dashboard and Jaeger next to the gateway.

Telemetry never takes part in a decision: admission, pricing, limits and streaming behave the same with it on or off, and a collector that is down never slows a request. Nothing it produces carries prompt or response content, a raw end-user ID or a user hash.

## Settings

```yaml
telemetry:
  metrics_enabled: true                 # default true
  otel_enabled: false                   # default false
  otel_endpoint: http://jaeger:4318     # required when otel_enabled is true
```

`otel_endpoint` is the base URL of an OTLP/HTTP receiver. `/v1/traces` is appended unless the path already ends with it, so `http://jaeger:4318` and `https://otel.example/v1/traces` both work. The URL must not hold credentials, a query or a fragment. HTTPS uses normal certificate validation. Plain HTTP is accepted only for `localhost`, loopback or private IP literals, and single-label names such as a Compose service; even then the exporter refuses to connect when the name resolves to a public address. Redirects and proxy environment variables are never followed. An invalid endpoint is a validation error: a reload keeps the previous configuration.

All three settings apply on reload, without a restart:

- Each request uses the telemetry configuration of the generation it started on. A request already running keeps its tracer until it ends, so a stream started before a reload keeps its trace.
- A new endpoint creates a new exporter. At most two exist at once: the current one and one finishing the requests that started before the change. Another reload in that window retires the oldest; its requests go on without spans, and the lost spans are counted.
- `metrics_enabled: false` makes `/metrics` answer 404 and stops counting. The values counted so far are kept: enabling again continues from them, with a gap and no backfill.

## Metrics

`GET /metrics` exists only on the admin listener (`127.0.0.1:8081` by default), never on the proxy port. It answers the Prometheus text format (`text/plain; version=0.0.4`) with `Cache-Control: no-store`.

```sh
curl -H "Authorization: Bearer $COMPORTA_ADMIN_TOKEN" http://127.0.0.1:8081/metrics
```

- Only the admin token, as `Authorization: Bearer`, is accepted. App keys and admin panel session cookies are refused with 401 `invalid admin token`. Failed attempts share the admin API protection: the tenth failure from one address within 60 seconds blocks it for 5 minutes with 429 and `Retry-After`.
- The Host allowlist applies first, as for every route. Only `GET` on exactly `/metrics` exists: other methods, a query string or a body are refused.
- A collection failure answers 503 `metrics unavailable` and is counted.

Counters belong to the process: they start at zero on every start, which `rate()` and `increase()` handle. SQLite remains the source of record for money; the exposed values are estimates for monitoring.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `comporta_requests_total` | counter | app, model, operation, outcome | One per logical proxy request. A fallback is one request with several attempts. |
| `comporta_request_duration_seconds` | histogram | app, operation | From the route handler until the request ended, streaming included. |
| `comporta_ttfb_seconds` | histogram | app, model | Until the first content reached the client. Requests that delivered nothing have none. |
| `comporta_upstream_attempts_total` | counter | app, provider, model, result | Attempts actually sent: `ok`, `client_error`, `server_error`, `transport_error`, `canceled`. |
| `comporta_upstream_duration_seconds` | histogram | provider, model | Each attempt sent, until its stream ended, failed or was canceled. |
| `comporta_cost_microusd_total` | counter | app, provider, model | Estimated cost confirmed by the metering, in microdollars, every attempt included. |
| `comporta_tokens_total` | counter | app, provider, model, direction | `input`, `output`, `cache_read`, `cache_write`: disjoint, as the metering records them. |
| `comporta_usage_missing_total` | counter | app, provider, model | Attempts that ended without complete usage; their cost is the conservative floor the metering charged. |
| `comporta_blocks_total` | counter | app, type, scope | Terminal refusals by primary reason. |
| `comporta_cache_hits_total` | counter | app, model | Responses served from the response cache. |
| `comporta_fallbacks_total` | counter | app, cause | Transitions to an admitted fallback model, even when that model then fails. |
| `comporta_budget_used_ratio` | gauge | app, scope, dimension, kind | Spent divided by the ceiling. |
| `comporta_budget_reserved_ratio` | gauge | app, scope, dimension, kind | Reserved by requests in flight, divided by the ceiling. |
| `comporta_budget_users_observed` | gauge | app | Users active in a window with a per-user ceiling. |
| `comporta_limit_state_unavailable` | gauge | app | 1 when the budget state is unknown or stale. |
| `comporta_events_total` | counter | type | Operational events received, best effort. |
| `comporta_event_notifications_lost_total` | counter | | Event notifications this process could not receive. |
| `comporta_telemetry_failures_total` | counter | component, reason | Failures of the telemetry itself. |
| `comporta_telemetry_dropped_spans_total` | counter | reason | `queue_full`, `export_failed`, `reload_retired`, `shutdown_timeout`. |
| `comporta_telemetry_label_overflow_total` | counter | family | Observations aggregated into the overflow series. |

Label values:

- `operation`: `generation`, `count_tokens` or `models`. Counting tokens is never billable consumption.
- `outcome`, the first that applies: `canceled`, `error`, `loop_cut`, `blocked`, `cached`, `fallback`, `downgraded`, `ok`. A stream that fails after its 200 was sent is `error` or `canceled`.
- `type` of a block: `rate`, `concurrency`, `budget`, `hard_cap`, `reservation`, `loop`, `sensitive`, `paused`, `authentication`, `validation`; `scope`: `app`, `user` or `request`. Failures of the gateway's own storage or limit state are errors, not blocks.
- `app`, `provider` and `model` are names from the configuration only. A request refused before authentication counts under `app="unauthenticated"`, and a model not declared by any provider under `model="unknown"`; text sent by a client never becomes a label.
- Durations are in seconds, with buckets from 5 ms to 300 s (TTFB stops at 60 s).

Each family keeps at most 1024 label combinations for the life of the process, including names a reload later removed. Further combinations are added to one series whose labels are all `overflow`, so totals stay right while detail is lost, and `comporta_telemetry_label_overflow_total` says so. Budget gauges above that limit are omitted rather than merged.

Money is accumulated as integers and converted only on exposition; Prometheus stores floats, which lose precision on very large totals. Divide by 1,000,000 for dollars.

### Budget gauges

`scope="app"` is the app's own spend over its ceiling. `scope="user_max"` is the largest ratio of any single user active in the current window, never a sum over users and never a series per user. Reserved amounts are reported apart from spend. A ceiling that is not configured has no series; a per-user ceiling without active users is 0. Forgotten users leave the aggregate at once.

The gauges are read from the limit coordinator every 5 seconds, never during a scrape. When the limit state is degraded or unknown, or the last reading is older than 15 seconds, the ratios of the app are withheld and `comporta_limit_state_unavailable` is 1: an unknown budget is never shown as free.

### Events

`comporta_events_total` counts the operational events of the bus by type (`request_completed`, `blocked`, `cached`, `fallback`, `downgraded`, `warned`, `loop_cut`, `config_changed`, `config_reload_failed`, `paused`, `sensitive_data_detected`, `limit_state_unavailable`, `limit_state_recovered`). The bus may drop notifications under load, which `comporta_event_notifications_lost_total` reports. Request, cost, cache and fallback counters never depend on it.

## Traces

With `otel_enabled: true`, every request that reaches a proxy route handler produces one trace, refused requests included. Health, admin and scrape requests do not, nor requests refused by Host or path before the handler. Sampling is 100%.

- The root span `comporta.request` (kind SERVER) starts before authentication and ends after the request's accounting.
- One INTERNAL span per stage that actually ran: `authenticate`, `validate`, `identity`, `pause`, `route`, `sensitive`, `loop_guard`, `admit`, `cache_lookup`, `fallback`, `deliver`, `settle`. A stage after a refusal never gets a span. Repeated stages carry `comporta.attempt`.
- A generation has one CLIENT span named `chat <requested model>` for the logical operation, with one `upstream_attempt` child per attempt sent, each covering the attempt until its stream ended, failed or was canceled. Fallbacks therefore stay in one trace. A cache hit has `cache_lookup`, `deliver` and `settle` and no attempt.

Attributes come from a fixed list: `gen_ai.operation.name`, `gen_ai.provider.name`, `gen_ai.request.model`, `gen_ai.response.model`, `gen_ai.usage.input_tokens`, `gen_ai.usage.output_tokens`, `gen_ai.request.stream`, `error.type`, `http.response.status_code`, `comporta.app`, `comporta.operation`, `comporta.attempt`, `comporta.outcome`, `comporta.cost_microusd`, `comporta.cache_hit`, `comporta.limit.type`, `comporta.limit.scope`. They follow the OpenTelemetry semantic conventions revision 1.39.0, GenAI included. Model names are the configured ones, never the text a provider returned. `gen_ai.usage.input_tokens` adds input and cache tokens once each. The logical span sums usage and cost over every attempt; each attempt keeps its own. No content, message, tool name, user ID, user hash, request ID or exception text is ever recorded.

The resource holds only `service.name=comporta` and `service.version`; no host, process or environment detector runs. Incoming `traceparent`, `tracestate` and `baggage` headers are ignored: every request starts a new trace.

Spans wait in a queue of 2048 per exporter and leave in batches of up to 512 every 5 seconds, each export bounded by 5 seconds, retries included. Nothing waits on the network in the request path: a full queue drops the span, a failed export drops its batch, and both are counted. Failures are logged with fixed fields at most once a minute. On shutdown, the gateway stops accepting requests, drains them, then gives the exporters 5 seconds in total; what remains is counted as `shutdown_timeout`.

## Logs

Logs are JSON lines from `log/slog` on stderr. Telemetry diagnostics use the message `telemetry degraded` with only `category` and `reason`; they never include an exporter error, a URL, headers, a client address, a client request ID or content.

## Compose profile

`docker compose --profile observability up` starts, next to the gateway:

| Service | URL on the host | Role |
|---|---|---|
| Prometheus | http://127.0.0.1:9090 | Scrapes `comporta:8081/metrics` every 15 s with the admin token and evaluates the example rules |
| Grafana | http://127.0.0.1:3000 | Prometheus and Jaeger datasources and the Comporta dashboard, provisioned |
| Jaeger | http://127.0.0.1:16686 | Receives OTLP/HTTP from the gateway inside the Compose network and keeps traces in memory |

Every image is pinned by digest. The UIs are published on the host's loopback only and the OTLP receiver is not published at all. Like the gateway, the containers run with a read-only filesystem, no capabilities and `no-new-privileges`, each writing only to its own volume or tmpfs. Without the profile, `docker compose up` runs the gateway alone and needs none of the files below.

### Preparation

1. Pick the configuration: `examples/comporta-observability.yaml` uses Anthropic and `ANTHROPIC_API_KEY`. It adds `comporta` to `allowed_hosts`, which Prometheus sends as Host, and points tracing at `http://jaeger:4318`.

   ```sh
   export COMPORTA_CONFIG=./examples/comporta-observability.yaml
   ```

2. Create an app key:

   ```sh
   docker compose run --rm --no-deps comporta keys create my-app --config /etc/comporta/comporta.yaml
   ```

3. Get the admin token with the gateway stopped. On a new volume the first `serve` prints it once; you can also print a new one at any time with:

   ```sh
   docker compose run --rm --no-deps comporta admin reset-token --config /etc/comporta/comporta.yaml
   ```

4. Store the token and a Grafana password in private files outside the repository, without a trailing newline, and give their paths to Compose. Keep the directory private (`chmod 700`); the files must be readable by the containers' users, which differ from yours:

   ```sh
   mkdir -p ~/.comporta && chmod 700 ~/.comporta
   printf '%s' "<admin token>" > ~/.comporta/metrics-token
   printf '%s' "<a password you choose>" > ~/.comporta/grafana-password
   chmod 644 ~/.comporta/metrics-token ~/.comporta/grafana-password
   export COMPORTA_METRICS_TOKEN_FILE=~/.comporta/metrics-token
   export GRAFANA_ADMIN_PASSWORD_FILE=~/.comporta/grafana-password
   ```

   Prometheus mounts only the token file, read-only, through `authorization.credentials_file`; it never sees the gateway's data volume. Compose never creates these files: a missing one stops the profile from starting. Grafana's user is `admin` unless `GRAFANA_ADMIN_USER` says otherwise; there is no published default password.

5. Start:

   ```sh
   docker compose --profile observability up -d
   ```

Open Grafana, log in and the Comporta dashboard is the home page. It refreshes every 10 seconds; counters and gauges appear on the first scrape after traffic, within 15 seconds, and rates once they have a minute of data. Without traffic, panels show zero or nothing, never synthetic data. Jaeger lists the `comporta` service with one trace per request. Grafana migrates its database on its very first start, which can take a few minutes on some Docker hosts.

### Trying it without a provider

`examples/observability/compose.fake-provider.yaml` adds the built-in fake provider at a fixed private address of the Compose network, and `examples/observability/comporta-fake.yaml` points the gateway at it:

```sh
export COMPORTA_CONFIG=./examples/observability/comporta-fake.yaml
docker compose -f compose.yaml -f examples/observability/compose.fake-provider.yaml --profile observability up -d
```

Use the same `-f` options for the `run` commands of the preparation.

### Lost token, rotation and reload

If the admin token is lost or replaced, stop the gateway, run `admin reset-token`, write the new token to the file and restart: Prometheus reads the file again on every scrape. The digest in the data directory cannot give the token back. Telemetry settings reload like the rest of the configuration; Prometheus keeps scraping while metrics are disabled and gets 404 until they are enabled again.

## Dashboard and alerts

`examples/observability/grafana/comporta.json` shows requests per second by app and outcome, upstream attempts by result, request duration p50 and p95, TTFB p95, estimated cost per period and per minute, tokens by direction, blocks by type, cache hits and fallbacks, budget used and reserved ratios, and the health of the telemetry itself. Filters select the app and the model. Panels about requests, attempts and consumption are labeled apart, since one request may make several attempts.

`examples/observability/alerts.yml` holds example rules, recommended defaults to edit as needed, not blocking policies:

- upstream errors (`server_error` or `transport_error`) above 5% of the attempts of an app and provider over 5 minutes, with at least 20 attempts, for 2 minutes;
- request duration p95 above 10 seconds over 5 minutes, for 5 minutes;
- `comporta_budget_used_ratio` above 0.9 for 1 minute.

A period without traffic never fires. The profile has no Alertmanager: firing alerts show in Prometheus only. `examples/observability/alerts_test.yml` tests the rules with promtool from the pinned Prometheus image: `task test-alerts`.

## Limitations

- Metrics and traces describe one process. Several gateways need one scrape target each, and their counters are independent.
- Counters restart at zero with the process; values counted while metrics were disabled are not recovered.
- Exposed money is an estimate in float; the database keeps the exact record.
- Spans are not stored on disk: an unreachable collector loses them, counted. Jaeger in the profile keeps traces in memory only.
- Only OTLP over HTTP with protobuf is supported for traces. Logs are not exported over OTLP.
- Trace context from clients is ignored; distributed tracing across services is not supported.
