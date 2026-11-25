# Comporta

Open-source LLM gateway, in development.

**Current state:** this repository delivers the configuration runtime, the app key lifecycle, the request proxy, usage metering, per-app and per-user limits and budgets, a loop guard for runaway agents, sensitive data detection with redaction and blocking, limit policies (block, downgrade and warn, allowed models and output caps), fallback routing across a chain of models of the same API format, an exact-match response cache in memory, operator commands (pause, resume, user deletion and usage queries) through an authenticated admin API, a read-only admin panel with live updates, alerts to signed webhooks, Discord, Slack and ntfy, and observability: Prometheus metrics on the admin listener, optional OpenTelemetry traces and a Compose profile with Prometheus, Grafana and Jaeger. `comporta serve` opens the proxy and admin listeners and forwards SDK traffic to the configured providers, with authentication, streaming, hot reload and a built-in fake provider.

**Not implemented yet:** fallback to a model of another API format. Its settings are validated and made available **without taking effect**, so do not rely on it as protection.

## Commands

```sh
comporta config validate [path]                    # structure and references, no secrets needed
comporta config validate [path] --resolve-secrets  # also reads the environment, .env and secret files
comporta keys create <app> [--config path]         # creates a key and prints it once
comporta keys rotate <app> [--config path]         # adds a key, keeping the others valid
comporta keys list [--config path]                 # lists keys without revealing them
comporta keys revoke <key-id> [--config path]      # revokes a key
comporta serve [--config path]                     # runs the proxy and admin listeners until interrupted
comporta serve --reset-user-history                # recovery from a lost hmac.key, see End users
comporta fake-provider [--port 9090] [--bind ip] [--scenario name]  # local imitation of both provider APIs
comporta pause <app> | --all                       # stops new requests until resumed, see Operations
comporta resume <app> | --all                      # restores traffic
comporta users forget <user-id>                    # deletes every record of an end user
comporta usage [--app a] [--user id | --anonymous] [--since 7d | --from d --through d] [--json]
comporta admin reset-token                         # new admin token, gateway stopped
```

`pause`, `resume`, `users forget` and `usage` talk to the running gateway through the admin API and need the admin token in `COMPORTA_ADMIN_TOKEN` or `--token`. See Operations.

`config validate` exits 0 when the file is valid and 1 with a list of errors otherwise. Results go to stdout and diagnostics to stderr. The structural mode is meant for CI: it checks the format of secret references, but it does not prove that a secret exists or that a provider works.

The path comes from the positional argument or `--config`, and defaults to `comporta.yaml` in the working directory. Giving both with different files is an error.

## Getting started

```sh
go build -o comporta ./cmd/comporta
cp examples/comporta.yaml comporta.yaml
echo 'ANTHROPIC_API_KEY=your-key' > .env
./comporta keys create my-app
./comporta serve
```

Then point an existing SDK at the gateway. Nothing else in the application changes:

```python
client = anthropic.Anthropic(base_url="http://localhost:8080", api_key="cmp_...")
client = openai.OpenAI(base_url="http://localhost:8080/v1", api_key="cmp_...")
```

[`examples/comporta.yaml`](examples/comporta.yaml) is minimal and [`examples/comporta-full.yaml`](examples/comporta-full.yaml) shows every field. Prices in the examples are illustrative. The published JSON Schema, [`schema/comporta.schema.json`](schema/comporta.schema.json), gives editors autocomplete and validation, and documents each default. Relational rules, such as model references and hard caps, are enforced only by `config validate`.

## Configuration format

The file is one YAML document of at most 1,000,000 bytes. Unknown fields, duplicate keys, anchors, aliases, merge keys, explicit tags and `null` are rejected.

- **Timezone:** the default is the system timezone when it has an IANA name. Windows zone names are not IANA names, so on Windows set `timezone` in the file or `TZ` in the environment.
- **Money:** plain decimals with at most six decimal places, converted exactly. Scientific notation and quoted amounts are rejected.
- **Hard caps:** with `on_limit: downgrade`, an omitted dimension defaults to 120% of its budget, rounded up. With `on_limit: warn`, an omitted dimension has no hard ceiling. See [Limit policies](#limit-policies).

## Secrets

Secrets are never written in the file. A literal value in any field named `api_key` makes startup and reload fail.

```yaml
api_key: ${ANTHROPIC_API_KEY}   # the reference must fill the whole value
api_key_file: /run/secrets/key  # or a file; use only one of the two
```

- Variables come from the process environment and from a `.env` next to the configuration file. The process environment wins, even when a variable is set to empty. A missing or empty variable is an error.
- `.env` is read once at startup. Changing it requires a restart.
- Relative secret paths are resolved from the configuration file's directory. Files may be at most 64 KiB, and one final line break is removed. Symlinks are followed.
- Alert channel URLs, signing secrets and tokens follow the same rules, with the fields `url`/`url_file`, `signing_secret`/`signing_secret_file` and `token`/`token_file`.

Logs, status and change events show secrets only as `[REDACTED]`. At startup, a warning is logged when the configuration file or data directory is readable by other OS users. Comporta never changes permissions by itself.

## Hot reload

Comporta watches the file's directory, so editors that save atomically keep working, on Windows too. A valid change is active within 2 seconds on a local disk, after a 500 ms debounce. A periodic check recovers events the operating system dropped, but this timing is not guaranteed on remote volumes.

```text
config reloaded: 3 apps, 2 changes (bot-discord.budget.usd_per_day 2.00 → 5.00)
config reload rejected: apps.bot-discord.on_limit: must be one of block, downgrade, warn (previous configuration still active)
```

- **Invalid edits** are rejected as a whole and the previous configuration keeps running.
- **Restart-only fields:** `data_dir` and the `bind` and `port` of both listeners. An edit that changes one of them is rejected entirely, including its other changes.
- **Deleted file:** the previous configuration stays active, a warning is logged every 60 seconds and the file is picked up again when it returns.
- **Secret rotation:** changing only a secret file does not trigger a reload. Save the configuration file, even unchanged, to reread secret files.
- **At startup** there is no previous configuration, so any failure aborts.

## App keys

Apps authenticate with keys that Comporta generates. A key is shown once, when it is created, and only its SHA-256 hash is stored, so a lost key cannot be recovered: revoke it and create another. [ADR 0001](docs/adr/0001-app-key-hashing.md) explains why plain SHA-256 is the right hash here.

```text
$ comporta keys create my-app
Key for my-app (shown only once): cmp_<43 base62 characters>
Store it in the app's environment. Key ID: k_<32 hex characters>

$ comporta keys list
KEY ID  APP     STATUS  CREATED AT            LAST USED AT
k_<id>  my-app  active  2026-11-06T12:00:00Z  never
```

These commands read the configuration without resolving secrets and write directly to the data directory, so they need no provider credentials and work with or without a running service. They exit 0 on success and 1 otherwise. An app name must be defined in the configuration.

- **Provisioning:** `serve` refuses to start without `keys.json` and never creates it, so run `keys create` first. A store that exists with no active key is valid, and every credential is rejected.
- **Rotation:** each app can have 2 active keys. `keys rotate` adds the second one. Move the app to it, then `keys revoke` the old one. Rotation never revokes anything by itself.
- **Revocation:** a running service applies it within 2 seconds on a local disk, without a restart. Operations already authenticated are not interrupted. Revoked records stay in the file, which holds at most 10,000 records, so that a later attempt reports the key as revoked.
- **Recovery:** while `keys.json` is missing, invalid or too open, authentication is blocked with 503 regardless of `fail_mode`, and resumes when the file is valid again. Comporta never recreates a damaged file: restore it from a backup, or remove it and create new keys.
- **Last used:** the time is saved every 5 seconds and on shutdown, so it is approximate and a crash can lose the latest update.

### Files

`keys.json`, `keys.lock`, `hmac.key`, `hmac.lock` and a data directory created by these commands are private to the user running Comporta: modes 0600 and 0700 on Linux and macOS, and a protected ACL limited to that user, SYSTEM and Administrators on Windows, which is equivalent in effect but is not POSIX 0600. An existing file with broader access, or a link in its place, is refused rather than fixed. An existing data directory is left as it is. Writes are atomic and coordinated by an operating system lock, which requires a local disk: network volumes are not supported.

### Authentication limits

After 20 failed attempts from one address within 60 seconds, that address gets 429 with `Retry-After` for 5 minutes, even with a valid key. This protection is held in memory per process and a restart clears it. It uses only the address of the connection and ignores `Forwarded`, `X-Forwarded-For` and `X-Real-IP`, so all clients behind one reverse proxy share a single counter and can block each other.

On Anthropic routes a key may come in `x-api-key` or as a bearer token, because SDK versions differ in which one they send for the same `api_key`. OpenAI routes take a bearer token only. Sending both, or repeating either, is refused.

## Proxy

The proxy listens on `127.0.0.1:8080` and the admin listener on `127.0.0.1:8081`. They are separate: no admin route exists on the proxy, and no conversation route on the admin listener. Both refuse a `Host` outside `localhost`, `127.0.0.1`, `[::1]` and `listeners.allowed_hosts` with 421, which protects against DNS rebinding. Forwarded headers are never trusted.

| Endpoint | Format | Notes |
|---|---|---|
| `POST /v1/messages` | Anthropic | Normal and streaming |
| `POST /v1/messages/count_tokens` | Anthropic | Advisory count, never treated as consumption |
| `POST /v1/chat/completions` | OpenAI, also used for Ollama | Normal and streaming |
| `POST /v1/responses` | OpenAI Responses API, conversational subset | Normal and streaming, always translated, see [Cross-provider translation](#cross-provider-translation) |
| `GET /v1/models` | Chosen by the client | Local list, see below |

Every other path or method returns 404 `endpoint not allowed by Comporta`, without forwarding anything, so an app cannot use your provider key for files, batches or admin APIs. The admin listener has only `GET /healthz`, which answers `{"status":"ok"}` and says nothing about providers.

**What reaches the provider:** when the model's provider speaks the route's format, the body exactly as received, byte for byte, including unknown fields, with documented patches. End-user fields are replaced by a pseudonym, as [End users](#end-users) describes. On OpenAI-format streaming, `stream_options.include_usage` is set to `true` so that usage can be measured, and if the client did not ask for it, the extra usage chunk is removed before delivery. Generation output is capped by the app's `max_tokens`: a higher value is reduced and an absent OpenAI limit is inserted. An existing `max_completion_tokens` field keeps its name, and the cap bounds the admission quote. With `on_limit: downgrade` the `model` field can be replaced, see [Limit policies](#limit-policies). Only `content-type`, `anthropic-version` and `anthropic-beta` are forwarded; every other client header is dropped, and the app key is replaced by the provider key. The destination comes only from the configuration. Redirects are never followed, proxy environment variables are ignored, and a remote provider name that resolves to a private, loopback or link-local address is refused. A base URL may end in `/v1` or not; the same path results. A model of another format gets the request translated to its provider's protocol, as [Cross-provider translation](#cross-provider-translation) describes.

**Errors** use the format of the route and carry a `comporta_code`. Provider error messages are never passed on, because they can echo prompts: status and category are preserved and the message is rebuilt. Inside a stream already started, the failure is an error event followed by closing the stream, so a truncated stream never looks like a success.

| `comporta_code` | Status | Meaning |
|---|---|---|
| `endpoint_not_allowed` | 404 | Route or method outside the list |
| `host_not_allowed` | 421 | `Host` not allowed |
| `invalid_request` | 400 | Invalid JSON, types, duplicate keys or query |
| `user_required`, `user_invalid`, `user_email_not_allowed`, `user_document_not_allowed` | 400 | End-user identifier missing or refused, see [End users](#end-users) |
| `model_unknown`, `provider_format_mismatch` | 400 | Model not declared, or declared in another API format on `count_tokens` or as a downgrade target |
| `model_not_allowed` | 403 | Model outside the app's `allowed_models` |
| `price_unknown`, `pricing_overflow` | 400 | No effective price, or cost outside the supported integer range |
| `rate_limited`, `concurrency_exceeded`, `budget_exceeded`, `budget_reserved`, `hard_cap_exceeded` | 429 | A limit or budget, see [Limits and budgets](#limits-and-budgets) |
| `limits_unavailable` | 503 | Limit state cannot be verified under `fail_closed` |
| `usage_storage_unavailable` | 503 | No accounting capacity, or durable admission could not be confirmed; provider is not called |
| `unsupported_media_type` | 415 | Body is not uncompressed JSON |
| `request_too_large` | 413 | Body over `max_body_bytes` |
| `provider_unreachable`, `provider_redirect`, `invalid_upstream_response` | 502 | Provider could not be used |
| `provider_timeout` | 504 | A deadline expired |
| `provider_rate_limited`, `provider_overloaded`, `provider_error` | Provider's | Provider refused or failed |
| `no_eligible_fallback_model` | 502 | No model of the request's chain can represent it in its protocol, see [Cross-provider translation](#cross-provider-translation) |
| `response_translation_failed` | 502 | A provider answer could not be converted to the client's format |
| `internal_error` | 500 | A panic, limited to that request and logged without content |

**Limits:** request body 10 MB, response headers from the provider 60 s, non-streaming requests 300 s in total including reading the body, stream silence 60 s, all configurable under `listeners`. Fixed: 64 MB per non-streaming response, 10 MB per stream event, JSON nesting of 128 levels, 64 KiB of request headers. On shutdown, requests get 10 s to finish and are then canceled, so a long stream can end when the service stops.

**Duplicate JSON keys are refused** anywhere in the body, because the gateway and the provider could otherwise read different values. Each client connection carries one request, which is what lets a reload change the header timeout, so clients open a new connection per request. Timeouts and allowed hosts apply to new requests after a reload; requests in flight keep the configuration and identity they started with. Connections to providers use HTTP/1.1.

### Model catalog

`GET /v1/models` lists the app's `allowed_models` in the format of the client, without calling a provider, so it costs nothing and says nothing about availability. `created` and `created_at` hold fixed values because the configuration has no release dates. The Anthropic format supports `limit`, `after_id` and `before_id`. Calls are held to the same list: any other model gets 403 `model_not_allowed`.

### Usage and privacy

Each provider call produces a private SQLite record with counters, timing, categories and the user's app-scoped pseudonym, never content, raw user identifiers, request digests or raw errors. A counter the provider did not report stays unknown. Observed counters are preserved even when the final cost must use a conservative estimate. `count_tokens` has its own operation and advisory counter, with zero generation cost and zero consumption tokens. Listing models is free and creates no consumption.

Prompts and responses stay in memory for the duration of the request and are not logged, and no option enables logging them. A privacy test sends marked prompts and identifiers through the gateway, including error paths, and fails if a marker appears in logs, answers built by the gateway, results, database artifacts or generated files. It requires nonempty persisted usage, aggregates and events and scans the DB/WAL before and after recovery. [`tests/privacy/coverage.json`](tests/privacy/coverage.json) tracks these channels, including the metrics, the traces and the bodies delivered to alert receivers.

### Prices and estimated cost

Money is represented as integer microdollars. Token rates are integer microdollars per million tokens. The exact products of input, output, cache write and cache read are added before rounding up once per attempt. OpenAI cached input is excluded from regular input; reasoning tokens already included in output are not charged again. Anthropic cache writes preserve their 5-minute and 1-hour breakdown. If only the write total is known, the highest applicable write rate is used. Reviewed long-context tiers apply to the entire request, including cached input when selecting the tier.

`pricing` YAML entries replace a model's whole embedded entry, including tiers. Only exact reviewed model IDs are recognized. An OpenAI-compatible local/private provider with no override and no embedded entry is treated as free; a private address alone does not make an Anthropic-format provider free. Configure an explicit override for a paid local proxy. An unpriced generation returns 400 `no price defined for model "x"; add it under pricing in comporta.yaml` before forwarding.

```sh
comporta pricing check
COMPORTA_ADMIN_TOKEN=... comporta usage --app my-app --since 7d --config comporta.yaml
```

The price catalog is dated and never refreshed over the network at runtime. `pricing check` warns when its age exceeds 60 days; update the binary or supply YAML overrides. All monetary output is **estimated cost**, not an invoice. Taxes, negotiated discounts, regional modifiers and extra paid tools are not represented by the four token dimensions.

Admission estimates input as `ceil(max(original_body_bytes, effective_body_bytes)/3) + 1600*image_count`. This heuristic is tested against the synthetic fixture corpus, not a universal tokenizer guarantee. The quote freezes rates, source, catalog date, configuration generation, output cap and the app's timezone. Its input-only price is distinct from its conservative reservation, which includes capped output and the highest applicable input/cache/tier rates. Real usage above the quote is retained and marked, never truncated. Partial or missing usage settles at the greater of known cost and the frozen conservative quote.

### Durable usage and retention

`data_dir/usage.sqlite` uses SQLite without CGO, versioned transactional migrations, WAL and `synchronous=FULL`. An exclusive `usage.lock` protects the directory from another service or offline reset. DB/WAL/SHM and the directory must be private. Use a local filesystem with working locks and sync; network volumes and replicas sharing this store are unsupported.

Every attempt is admitted durably before contacting the provider. There are 50,000 pending slots, including active attempts and completions awaiting commit. A full queue or failed admission write returns 503 without forwarding. Completed results are journaled and materialized in batches of at most 1,000, every two seconds or when a batch fills. Records, app/user daily totals, completion events and journal removal share one transaction. Failed writes retain the batch and its slots, with bounded retry and sanitized diagnostics. These slots reserve accounting capacity, not budget money.

After a crash, a completed journal entry preserves its exact reported result. An admission without a durable result produces a conservative recovered record, without repeating the provider call. This can account for an attempt that crashed before sending; it cannot reconstruct provider usage that never reached durable storage. Shutdown drains handlers, flushes and checkpoints with a ten-second accounting deadline; failure returns an error and leaves the journal recoverable. These guarantees rely on the filesystem honoring sync and locks and do not cover physical disk loss.

```yaml
storage:
  retention_days: 30  # 1 through 365; reload affects future maintenance
```

Hourly maintenance removes expired request/event/configuration details in batches, preserves pending journal entries, and never re-adds their totals. Daily aggregates are retained indefinitely, with the original civil day and timezone. `usage` queries the running gateway (see Operations): app totals and user totals are never added together, `--since 7d` includes today and the previous six civil days, and totals are an exact six-decimal USD amount. Finalizations may take two seconds to appear. Removed apps remain queryable using their historical timezones. Internal typed events have a durable history and a bounded live bus; slow consumers must resynchronize after overflow. Administrative APIs, policies, the panel, alerts and telemetry remain separate consumers.

### Limits and budgets

Per-minute limits, concurrency limits and calendar budgets apply to every app and, identically, to each of its end users. The anonymous bucket of an app is separate from identified users and from every other app.

```yaml
apps:
  bot-discord:
    timezone: America/Sao_Paulo   # defaults to the root timezone
    fail_mode: fail_closed        # or fail_open; defaults to the root fail_mode
    limits: {requests_per_minute: 600, tokens_per_minute: 200000, max_concurrent: 20}
    budget: {usd_per_day: 2.00, usd_per_month: 40.00, tokens_per_day: 2000000}
    hard_cap: {usd_per_day: 2.40}
    per_user:
      limits: {requests_per_minute: 10, max_concurrent: 4}
      budget: {usd_per_day: 0.20}
```

- **Sliding windows:** `requests_per_minute` and `tokens_per_minute` count admissions in the last 60 seconds of monotonic time, so with a limit of 60 the 61st call within any 60 second span is refused, across civil minute boundaries. A call exactly 60 seconds later no longer competes. Refused calls consume nothing.
- **Concurrency:** `max_concurrent` defaults to 20 per app and 4 per user and lasts until the attempt ends, including the whole stream.
- **Budgets:** daily budgets renew at 00:00 and monthly budgets on day 1 in the timezone of the app, using calendar rules (days of 23 or 25 hours, month ends, leap days), never fixed durations. Token budgets and `tokens_per_minute` count the four disjoint usage dimensions: input, output, cache write and cache read.
- **Reserve and settle:** before forwarding, the worst case (the cost of the frozen quote, and estimated input plus capped output in tokens) is reserved against every applicable dimension of the app and the user, atomically: one refusal changes nothing. After the response the reservation is replaced by the real cost. A cancellation or error settles what was consumed; when usage is incomplete, each dimension is charged the greater of what was observed and what was reserved. An attempt that was never sent releases its money and tokens but still counts as a request.
- **`count_tokens`** counts toward requests per minute and concurrency only. `GET /v1/models` counts toward nothing.
- **Order:** limits are evaluated as request rate, token rate, concurrency, hard caps, then budgets, app before user, and within budgets USD per day, USD per month, tokens per day, tokens per month. The first exceeded limit names the refusal. Hard caps use the same balances as budgets with their own ceilings.
- **Policies:** `on_limit` can downgrade or warn instead of blocking a budget, never a rate or concurrency limit; see [Limit policies](#limit-policies).

Refusals use the error envelope of each SDK, with HTTP 429 and `rate_limit_error`, plus `limit_name` (the configuration path), `scope` (`app` or `user`), `reason` and, when known, `renews_at` (UTC, RFC 3339):

| `comporta_code` | Cause |
|---|---|
| `rate_limited` | Request or token rate. `Retry-After` gives whole seconds when the release time is known; tokens still held by calls in flight have no known release time, so no promise is made |
| `concurrency_exceeded` | `too many concurrent requests for this user (limit 4)`, or the app variant; no `Retry-After` |
| `budget_exceeded` | Spent budget: the call does not fit even without other reservations, `reason: spent` |
| `budget_reserved` | The call would fit once calls in flight settle, `reason: reservation`, recorded apart from spent-budget blocks |
| `hard_cap_exceeded` | A hard cap, with `reason` spent or reservation |
| `limits_unavailable` | 503, `limits cannot be verified; request rejected (fail_closed)` |

```text
daily budget of $2.00 for app bot-discord would be exceeded (spent $1.97, reserved $0.00); renews at 00:00 America/Sao_Paulo (in 3h12m)
```

Every refusal before an attempt is stored as a blocked record with attempt 0, zero cost and its reason, plus a `blocked` event; it never enters the daily totals.

**Headers:** admitted calls carry `x-comporta-budget-remaining` (USD with six decimals, or tokens), `x-comporta-budget-reset` (Unix seconds), `x-comporta-budget-unit` (`usd` or `tokens`) and `x-comporta-budget-limit` (the configuration path, which tells a hard cap from a budget) for the applicable budget with the smallest fraction left after the reservation, compared exactly. They are sent with the status line, before the first stream event, and describe the moment of admission, not the balance when a stream ends. They are absent without an applicable budget or while limit state is degraded. Provider headers with the `x-comporta-` prefix are dropped.

**Durability:** the reservation is written in the same transaction as the usage journal entry, and the settlement in the same transaction as the result, before anything is forwarded or released in memory. Counters live in memory, a checkpoint is written every 5 seconds and at shutdown, and the ledger after the last checkpoint is replayed at startup. After a crash, reservations of calls in flight are settled from their recorded result or, without one, at their reserved value, and nothing is sent again. Recovery happens before the listeners open and can be repeated after another crash. On the first start over an existing usage database, budgets are seeded once from the daily totals, counting a day only partly inside the current window in full; token history that cannot be proven blocks that token budget until its next boundary instead of reading as zero.

**Clock and reload:** a wall clock that moves back more than 5 seconds logs a warning, at most one every 30 seconds; windows keep monotonic time and a closed budget window is never reopened, including after a restart with an earlier clock. A new ceiling applies to new admissions and keeps what was spent and reserved; removing a limit and adding it back keeps the history. A call that started under an older configuration generation than one the coordinator already applied is admitted again under the current one. A new timezone takes effect when the active windows of the configured budgets end, and the window then in force starts with the spending of the old windows it overlaps; the limit state reports the active and pending timezone.

**Fail modes:** with `fail_closed`, the default, a call is refused with 503 when limit state cannot be read or written, for example when a checkpoint fails or a failed settlement also fails its retry. With `fail_open` the call proceeds, its record is marked degraded, limits are still evaluated from memory, and a `limit_state_unavailable` event (severity warning, per app) and a log warning are emitted; `limit_state_recovered` follows once state is persisted again. `fail_open` never bypasses the usage journal: if usage cannot be admitted durably the call gets 503 `usage_storage_unavailable`. Arithmetic overflow and integrity failures never fail open. For apps in `fail_open`, the per-app event is also delivered as an alert, see [Alerts](#alerts).

**Guarantee and its limits:** admissions are serialized with their reservations, so when real usage stays within the reservation, concurrent calls cannot overrun a budget. A test fires 500 simultaneous calls at one budget in 100 consecutive rounds against SQLite and the fake provider and checks that spend never exceeds the budget by more than the largest single reservation; on the reference Windows machine every round stayed within the budget. When a provider reports more usage than estimated, the real cost is charged and the budget then refuses new calls, but several calls in flight that all exceed their estimate can together exceed the budget by more than one reservation; a test demonstrates this case. `fail_open` also suspends the guarantee while degraded. Token estimation is a heuristic, not a tokenizer.

**Measured cost:** admission and settlement reuse the two synchronous `synchronous=FULL` commits that metering already made, now carrying the reservation. On the reference Windows machine, sequential non-streaming calls through the fake provider measured a p95 of 6.7 ms against 2.0 ms without metering and limits, and streaming time to first byte a p95 of 3.6 ms against 2.6 ms. The coordinator alone takes about 27 µs per reserve and settle. The sustained load target of 200 requests per second for 5 minutes was not measured in this release.

**For later features:** alerts consume, as the panel and multiple instances do, typed decisions, a consistent state per app and user (used, reserved, limit, renewal, degradation, active and pending timezone) and a reservation handle with reserve, settle and release. `comporta users forget` removes a user's limit state together with the user's records, see Operations.

### Limit policies

`on_limit` chooses what happens when a calendar budget is exceeded. Rate, token rate and concurrency limits always block, whatever the policy: they protect capacity, not money.

```yaml
apps:
  bot-discord:
    allowed_models: [claude-sonnet-4-6, claude-haiku-4-5]
    max_tokens: 4096                 # default; larger values are lowered
    on_limit: downgrade              # block (default), downgrade or warn
    downgrade_to: claude-haiku-4-5
    budget: {usd_per_day: 2.00}
    hard_cap: {usd_per_day: 2.40}    # 120% of the budget by default with downgrade
```

- **`block`** answers 429 `budget_exceeded` with `limit_name`, `scope`, `reason` and `renews_at`, as [Limits and budgets](#limits-and-budgets) describes. A budget committed only by calls in flight still answers `budget_reserved`.
- **`downgrade`** sends `downgrade_to` instead of the requested model once a budget is exceeded, and continues while spend stays below the hard cap. Only the `model` field of the body changes, together with the documented output cap and identity patches; unknown fields and the content already sanitized by [sensitive data detection](#sensitive-data-detection) are kept. The target is priced on its own body, reserved and settled at its own price, and the answer carries `x-comporta-downgraded-from: <requested model>` with the status line, before the first stream event. The `model` field of the response is the one the provider returned; it is never rewritten. A request that already names the target is admitted as downgraded without the header. A target in another API format than the endpoint is refused with 400 `provider_format_mismatch` without calling any provider; `config validate` warns about such apps. If the target provider cannot be reached the answer is 502 and the original model is never tried instead.
- **`warn`** forwards the request unchanged, records it as `warned` and emits a durable `warned` event at admission. **Without `hard_cap`, warn has no financial ceiling**: spend can grow past the budget until the window renews. Capacity limits still apply.
- **Hard cap:** once spent plus reserved reaches the hard cap, every new generation of the app or user is refused with 429 `hard_cap_exceeded` under every policy, even one that would cost nothing, until the window renews: `daily hard cap of $2.40 for app site-suporte would be exceeded (spent $2.40, reserved $0.00); renews at 00:00 America/Sao_Paulo (in 3h12m)`. A request whose projection lands exactly on the cap is still admitted. With `downgrade`, the cap is checked against the target that is actually sent, so a cheaper target can fit where the original would not. A hard cap alone, without an exceeded budget, never triggers a downgrade.
- **Allowed models:** a request for a model outside `allowed_models` gets 403 `model "claude-opus-x" is not allowed for app bot-discord` before anything is reserved or sent, also on `count_tokens`. A downgrade never turns a forbidden model into an allowed one.
- **Output cap:** generation output is capped by `max_tokens`; a higher value is lowered, and an OpenAI request without an output limit gets it inserted. The cap applies to both the requested model and the target.
- **Unknown state:** a budget whose history cannot be proven is never relaxed by `warn` or `downgrade`. With `fail_mode: fail_open`, calls proceed from memory while limit state is unavailable, so the hard cap is not a guarantee during that time.

**Safety trade-off:** downgrade and fallback send the conversation to a different model, which can behave differently on safety, refusals and quality. Configuring `downgrade_to` or `fallback` means accepting that trade-off for the app. A lower price is never inferred from the model name: compare the prices of both models, and rely on the hard cap as the financial bound.

Every admission decided by a policy keeps the decision with its record: `PolicyOutcome` (`blocked`, `warned` or `downgraded`), the limit, scope, reason and renewal that triggered it, and the requested and used models, apart from the transport result, so a warned call that later failed upstream still shows its error. `warned` and `downgraded` events are written in the same transaction as the reservation, carry the request ID, app, limit and models but no user hash or content, and are removed by `comporta users forget` with the user's requests. A configuration reload that overtakes an admission prepares the request again on the new configuration (allowlist, output cap, sanitization, target and price) under the same request ID, at most three times before answering 503; a call already admitted keeps its model, body and price, and a stream in flight finishes even if the new cap is lower.

[`examples/limit-policies.yaml`](examples/limit-policies.yaml) shows the three modes on the fake provider, without real credentials.

### Fallback routing

`fallback` lists up to four models an app tries, in order, when the model it sent cannot answer. Each attempt is one call: a model is never called twice for the same request, and there is no backoff between attempts.

```yaml
apps:
  bot-discord:
    allowed_models: [claude-sonnet-4-5, claude-haiku-4-5, ollama/qwen2.5]
    fallback: [claude-sonnet-4-5, claude-haiku-4-5]
```

- **Where the chain starts:** after the model actually sent. A request for `claude-sonnet-4-5` continues with `claude-haiku-4-5`. A request for a model outside the chain tries the whole chain after it, so at most five models are sent per request. With `on_limit: downgrade`, the downgraded model is the starting point and the requested one is not tried.
- **What triggers it:** HTTP 429, any 5xx including Anthropic's 529, a connection error or a provider timeout, while nothing has been written to the client. A stream falls back only on an error event the gateway recognizes (rate limit or overload) before its first event reaches the client; once any event was delivered, even a heartbeat or a `message_start` without text, the error is forwarded and no other model is tried. HTTP 400, 401, 403, 404 and 413, refusals inside valid answers, broken or oversized answers, `count_tokens` and every refusal decided by the gateway itself (budgets, rate limits, allowlist, sensitive data, loop guard, pause) never fall back.
- **Formats can mix:** a chain entry in another format than the endpoint is translated, request and answer, as [Cross-provider translation](#cross-provider-translation) describes. An entry that cannot represent the request, such as any other format when the client sent `anthropic-beta`, is skipped before it is quoted or reserved and recorded as a `translation_skipped` event; it never counts as one of the five sends. `config validate` warns with `fallback_disabled` when no allowed model has an alternative in the chain.
- **Time:** a non-streaming request that can fall back has 120 seconds for all its attempts together, or less when `non_streaming_timeout` is lower, counted from admission. Streams keep the per-attempt header and idle timeouts. When the client leaves, the provider call ends at once and no other model is tried.
- **What the client sees:** the answer of the model that responded, with its own `model` field, and `x-comporta-fallback: claude-sonnet-4-5->claude-haiku-4-5 (529)` listing every admitted transition (`A->B (529); B->C (timeout)`). Causes are `429`, the 5xx status, `connection` or `timeout`; model names are percent-escaped. Headers of failed attempts, `Retry-After` included, never reach the client, and a provider cannot send its own `x-comporta-*` headers.

| `comporta_code` | Status | Cause |
|---|---|---|
| `fallback_exhausted` | 502 | Every eligible model failed: `all fallback models failed (claude-sonnet-4-5: 529, claude-haiku-4-5: timeout)` |
| `fallback_deadline_exceeded` | 504 | The shared deadline ran out, with the causes seen so far |
| `fallback_policy_target_exhausted` | 503 | The next attempt exceeded a budget and `on_limit: downgrade` requires a target that already failed in this request; it is not sent again |
| Limit codes (`budget_exceeded`, `hard_cap_exceeded`, ...) | 429 | The next attempt does not fit a limit; fallback never goes around a protection |
| `usage_storage_unavailable` | 503 | The result of the previous attempt could not be made durable; nothing else is sent |

Without a chain, or without an alternative, the original error is returned unchanged.

**Cost and limits:** every attempt is quoted with its own model's price, reserved before it is sent and settled before the next one is reserved; budgets, hard caps and `on_limit` apply to each attempt as to any call. A failed attempt whose provider reported no complete usage, including 429 and 529, is charged at its reservation, never as free. The request counts once for `requests_per_minute` and keeps its `max_concurrent` slot from the first attempt to the end; tokens per minute count each attempt at its own admission. A configuration reload in the middle of a chain does not change it: the next attempts keep the chain, prices and policy the request was admitted under.

**Records and events:** each attempt has its own record. Later attempts have outcome `fallback`, the model they replaced and the cause; the policy outcome stays separate. Only the attempt that ended the request counts as a request in the daily totals, all of them count as attempts, cost and tokens, and when the gateway answered something else than that attempt's own result (an exhausted chain, a refusal) the record keeps both. Each admitted transition writes one `fallback` event, in the same transaction as the next attempt's reservation, with the app, the request ID, the attempt index, `from_model`, `to_model` and the cause, but no user hash or content. It means the next model was admitted, not that it answered. After a crash, attempts in flight are charged at their reservation and the last one admitted becomes the terminal one; nothing is sent again. `comporta users forget` and retention remove these records with the user's requests and keep the app totals.

[`examples/fallback.yaml`](examples/fallback.yaml) runs a chain on the fake provider, without real credentials.

### Cross-provider translation

Requests are translated between Anthropic Messages, OpenAI Chat Completions and the conversational subset of the OpenAI Responses API: system prompts, multi-turn history, images, tool definitions, tool calls and results, tool choice, stop reasons, usage and streaming events. An app keeps its SDK and format; a model of another format receives its provider's protocol and the answer comes back in the client's, with `x-comporta-translated` naming the direction (`openai->anthropic`, `anthropic->openai` or `openai-chat->openai-responses`).

```python
from openai import OpenAI

client = OpenAI(base_url="http://127.0.0.1:8080/v1", api_key="cmp_...")
response = client.responses.create(model="claude-sonnet-4-5", input="Resuma o relatório.")
print(response.output_text)
```

Usage is settled from the provider's own counters and shown to the client in its format. Features without an equivalent, such as provider betas, server-side Responses state, built-in tools, reasoning or structured output, make a model ineligible for that request: it is skipped before any reservation and a `translation_skipped` event records the closed feature, or the request ends with 502 `no_eligible_fallback_model` when no model remains. Anthropic providers send `anthropic_version` (default `2026-10-21`) with translated requests. [docs/cross-provider-translation.md](docs/cross-provider-translation.md) has the full matrix, the Responses subset, every feature code and the stream error behavior; [`examples/translation.yaml`](examples/translation.yaml) runs a mixed chain on the fake provider.

### Response cache

The response cache answers a repeated deterministic request from memory, without calling the provider. It is off by default and enabled per app.

```yaml
cache:
  max_bytes: 256000000          # default and maximum, decimal bytes, shared by every app
apps:
  support:
    cache:
      enabled: true
      ttl_seconds: 600          # 1 to 3600, default 600
      shared_across_users: false
```

- **What is cached:** only `POST /v1/messages` and `POST /v1/chat/completions` with `temperature` present and equal to zero. Absent, `null` or any other value is never cached. `0` and `0.0` are both zero, but they are different requests.
- **Exact match:** an entry is reused only for the same app, the same end user (or the anonymous bucket of the app), the same provider, model, route and stream flag, the same protocol headers such as `anthropic-version`, and the same body bytes after the gateway's own changes. Field order, whitespace, unknown fields, tools, images and `stream_options` all count: reordering a JSON object is a miss. There is no semantic matching.
- **What is replayed:** the successful answer exactly as the client first received it, with status 200, its content type and `x-comporta-cache: hit`. A stream is replayed event by event in its original order, terminal included (`[DONE]` or `message_stop`), and the SDK reads it as a normal stream. The usage chunk is only present when the client asked for it. Only an answer that finished with a confirmed terminal, complete usage and no error, and whose record was made durable, is stored; errors, cut streams and answers from a later model of a fallback chain never are. Miss and bypass add no header.
- **Usage shown versus usage charged:** the replayed body keeps its original ids, timestamps and `usage`, so SDKs keep working. That usage belongs to the call that filled the cache. The hit itself consumes nothing: it is recorded with outcome `cached`, cost 0 and zero tokens, its reservation is released, and a `cached` event is written next to `request_completed`. `cache_read_tokens` and `cache_write_tokens` still mean the provider's own prompt caching.
- **Protections still apply:** the cache is read after authentication, pauses, deletion barriers, sensitive data inspection, the loop guard, the limit policy and the reservation. A hit counts for `requests_per_minute` and the loop guard and holds its `max_concurrent` slot until the replay ends. A budget or hard cap that cannot reserve the request refuses it even when the answer is cached: the cache never goes around a limit. With `on_limit: downgrade` the answer is stored under the model actually sent, and the response headers come from the current request.
- **Sensitive data:** a request whose content was redacted is never read from nor stored in the cache, with or without `rehydrate`, and neither is one whose inspection could not finish. `detect` alone, or a `redact` rule that found nothing, does not prevent caching.
- **`shared_across_users`:** lets any user of the same app receive an entry filled by another user, never across apps. The identity fields the gateway writes are left out of the match, but the answer itself may have been influenced by the first user, by their content or by a provider that echoes the user field. Enable it only for outputs that are not personal.
- **Memory:** one LRU across all apps, bounded by `max_bytes`. The budget counts the stored bytes, the captures in progress, replays still reading an evicted entry and a fixed overhead per entry and event; it is not a limit on the process memory. A single answer above 16 MB, or one that does not fit the free budget, is still delivered but not kept. Concurrent misses of the same request each call the provider.
- **Expiry and invalidation:** the TTL starts when the answer is stored, and a hit never extends it. Any applied reload empties the cache, whatever changed; a rejected or unchanged reload keeps it. Lowering `max_bytes` evicts at once what is not being replayed. `comporta users forget` empties every app the user had state in, shared entries included, and a capture that started before it is discarded.
- **Restart:** the cache lives in memory only. Nothing is written to the data directory, logs, events or the database, and every start begins empty.

**Hit rate:** for an app and a period, the number of generation requests recorded with outcome `cached` divided by all generation requests recorded, counting each request once (the terminal attempt, plus refusals before admission). It describes the real traffic in the retained records and is not an estimate of money saved.

[`examples/response-cache.yaml`](examples/response-cache.yaml) shows default isolation and a shared app on the fake provider, without real credentials.

### Loop guard

An agent stuck repeating one step can burn through a budget in minutes. The loop guard watches each end user of each app and cuts them for a while when their traffic looks like a loop, before any reservation is made and before anything reaches the provider.

```yaml
apps:
  agent-app:
    loop_guard:
      enabled: true            # false turns the guard off for this app
      similar_requests: 10     # similar requests allowed in the window
      similar_window: 30s
      requests_per_minute: 60  # requests allowed in any 60 second span
      cooldown: 120s
```

These are the defaults. The first 10 similar requests within 30 seconds go through and the 11th is cut; the first 60 requests within any 60 second span go through and the 61st is cut, whatever they contain. A cut lasts 120 seconds from the first refusal. Retries during the cooldown get the same answer and do not extend it; the first request after it ends is accepted and starts with empty windows. A request already admitted, including a stream in progress, is never interrupted.

**What "similar" means:** the signature is a digest of the last user message and the last tool result of the request, with the API format, not of the whole body. An agent that keeps appending to its conversation while sending back the same tool result is detected even though every body is larger than the last. JSON key order and escaping do not matter; message content, block order, number spelling and tool call IDs do, so an agent whose runtime issues a new tool call ID every turn is caught by the per-minute rule, not by repetition. A request without a user message or tool result is subject to the per-minute rule only.

**Who is counted:** each pair of app and pseudonymous user, so two keys of the same app share the count, and switching model or streaming does not reset it. Requests without a user ID share the anonymous subject of the app: many unidentified users of one app can be cut together. Require a user ID, raise the thresholds or disable the guard for such apps. Only `POST /v1/messages` and `POST /v1/chat/completions` are counted, once per HTTP request; `count_tokens` and `GET /v1/models` never are. Requests refused earlier (authentication, user ID, invalid body, pause, user deletion in progress) do not count.

**The answer** is HTTP 429 in the error envelope of each SDK, before any stream byte, as `application/json` with `Cache-Control: no-store` and a `Retry-After` in whole seconds, rounded up. No budget header is added.

```json
{"error":{"type":"rate_limit_error","message":"loop detected: 10 similar requests in 30s; requests from this user are paused for 120s","comporta_code":"loop_detected"}}
```

The per-minute variant reads `loop detected: more than 60 requests in 60s; ...`. Retries report the remaining seconds. Clients should stop retrying on `loop_detected` instead of retrying in a loop.

| `comporta_code` | Status | Cause |
|---|---|---|
| `loop_detected` | 429 | Repetition or request rate above the thresholds; `Retry-After` is the time left |
| `loop_guard_unavailable` | 503 | The guard reached its internal memory ceiling (100,000 users or 1,000,000 recorded arrivals); no protected user is evicted to make room |
| `usage_storage_unavailable` | 503 | The cut could not be recorded; the user stays in cooldown and nothing is forwarded |

**Records and events:** every cut request is stored with outcome `loop_cut`, attempt 0, status 429, zero cost and no reservation; it never enters the daily totals or the budgets. The first cut of an episode also writes one `loop_cut` event, in the same transaction, with the app, the user hash, the anonymous flag, the reason (`similar_requests` or `request_rate`), the count (11 or 61), the window and the cooldown in milliseconds. Retries in the same episode add records but no event. The event is announced to subscribers only after it is committed. The first cut of each app in every 15 minutes is also delivered as an alert, see [Alerts](#alerts).

**Memory only:** signatures and arrival times live in memory for at most their window and are never written to disk, logs, events or errors; a cut drops them at once. A restart clears all counters and cooldowns, so the guard complements budgets, which persist, and does not replace them. A configuration reload applies to the next request: disabling an app clears its state, including cooldowns; changing thresholds or windows discards the counts but keeps cooldowns already running; changing only the cooldown affects new cuts. `comporta users forget` clears the user's counters and cooldowns once the deletion has committed, and keeps them if it fails.

### Sensitive data detection

Comporta can look for personal data and credentials in the text of each request and count them, replace them before the provider sees them, or refuse the request. Everything happens in the gateway's memory: no external service, no extra dependency.

```yaml
apps:
  support:
    detection:
      cpf: redact
      cnpj: detect
      card: redact
      email: redact
      phone: detect
      pix: off
      secret: block
      rehydrate: true
```

Each category takes `off`, `detect` (count only), `redact` (replace before forwarding) or `block` (refuse the request). **Every category is `off` by default**, and `rehydrate` defaults to `false`. Detection stays off until the reference benchmark shows an added p95 under 5 ms on a 400 KB prompt, which has not been measured yet (see Performance below). We recommend turning it on explicitly: start with `detect` to see what your apps send, then move the categories you care about to `redact` or `block`. A reload changes only the requests that arrive after it; a request in progress keeps the policy it started with until its response is restored.

**What is detected:**

| Category | Rule | Not covered |
|---|---|---|
| `cpf` | 11 digits or `###.###.###-##`, both check digits, no repeated digit | A CPF glued to letters or to a longer number |
| `cnpj` | 14 digits or `##.###.###/####-##`, both check digits | The alphanumeric CNPJ |
| `card` | 13 to 19 digits, contiguous or in groups split by one kind of space or hyphen, Luhn | An order number that passes Luhn is reported as a card |
| `email` | Local part up to 64 characters of letters, digits and `._%+-`, a domain of valid labels and a letter TLD | Quoted and internationalized addresses; DNS is not checked |
| `phone` | Brazilian numbers: optional `+55`, a valid area code, 9 digits starting with 9 or 8 starting with 2 to 5, usual masks | Other countries; an 11-digit protocol number with a valid area code is reported as a phone |
| `pix` | A canonical version 4 UUID | Any v4 UUID is a candidate: nothing in the text proves a PIX registration, so trace IDs are reported too |
| `secret` | `sk-ant-` and `sk-` keys with 16 to 256 characters, AWS `AKIA` key IDs, GitHub `ghp_` and `github_pat_` tokens, structural JWTs (signature not verified), PEM private keys | Truncated keys, public keys and certificates |

Free-text names and addresses are not detected; there is no NER-based detection. These rules are product heuristics, not proof that a value is real or belongs to anyone.

**Where it looks:** in `POST /v1/messages`, `POST /v1/messages/count_tokens` and `POST /v1/chat/completions`, only the text a person or a tool wrote: Anthropic `system`, message content (strings and text blocks), `tool_use.input`, `tool_result` content and tool descriptions; OpenAI message content (strings and text parts, including `tool` messages), `function_call.arguments`, `tool_calls[].function.arguments` and function descriptions. Inside tool schemas only `description`, `enum`, `default` and `examples` are read, never property names. Tool arguments that hold JSON are opened and every string value is inspected; numbers, booleans and keys are not, so a CPF sent as a JSON number is not protected. Strings are read decoded, so `\u0040` counts as `@`, and fields are never joined: a number split across two messages is not found. Images, files, URLs, model names, roles, IDs, headers and unknown fields are never read. `GET /v1/models` is not inspected.

**Redaction** replaces each value with a placeholder such as `[CPF_1_00112233445566778899aabb]`: the category, an index starting at 1 per category in order of appearance (the same value keeps its index), and a 96-bit random suffix chosen per request and checked against the text. The provider receives `customer CPF [CPF_1_...] wants to cancel`. Overlapping matches keep the longest span; ties go to secret, cpf, cnpj, card, email, phone, pix. In `detect` mode the body is forwarded byte for byte. The reservation is estimated from the larger of the original and the redacted body, and the final cost always comes from the provider's usage. The loop guard signs the request before redaction, so new placeholder suffixes do not hide a loop.

**Blocking** refuses the whole request with HTTP 400 before any budget is reserved and before anything reaches the provider. The message names the category, or the kind of secret, and never the value:

```json
{"error":{"type":"invalid_request_error","message":"request contains a secret (anthropic_api_key); blocked by policy","comporta_code":"sensitive_data_blocked"}}
```

**Rehydration:** with `rehydrate: true`, the placeholders of the request are restored in successful non-streaming JSON answers: Anthropic text blocks and `tool_use.input`, OpenAI message content and tool call arguments. Usage, IDs, tool names, the model and unknown fields are left alone, a placeholder with another suffix stays as it is, and a restored value is never expanded again. Streams are relayed unchanged with the header `x-comporta-rehydration: unsupported-for-stream`, so a streaming client may see placeholders. Rehydration is not an output filter: personal data the model generates on its own is passed through.

**Failures:** the scan has a 50 ms budget for reading and checking all fields, and limits of 4,096 candidates, 1 MiB of distinct values and 64 KiB per secret candidate. The 50 ms is a ceiling for one request, not a latency promise. When the scan fails (deadline, limit, internal panic) and some category of the app is in `redact` or `block`, the request is refused; with `detect` only, it is forwarded unchanged and a fixed warning is logged.

| `comporta_code` | Status | Cause |
|---|---|---|
| `sensitive_data_blocked` | 400 | A category in `block` matched; no `Retry-After` |
| `sensitive_data_scan_failed` | 503 | The scan failed while a category is in `redact` or `block` |
| `usage_storage_unavailable` | 503 | The detection event or the refusal could not be recorded while a category is in `redact` or `block` |
| `response_rehydration_failed` | 502 | The answer could not be restored safely (ambiguous JSON or over 64 MB); the generated usage is still charged |

**Records and events:** values, positions, placeholders and hashes of values are never stored or logged. Each request with matches writes one `sensitive_data_detected` event before the request goes on, with the app, the user hash, the anonymous flag and a summary per category: `data_type`, `count`, `action` and, for secrets, `secret_kinds`. Its record carries the same summary. A refused request is stored with outcome `blocked`, attempt 0, zero cost and no reservation; a failed scan stores no counts, because they would be incomplete. `comporta users forget` and the identity key reset remove these events with the user's other rows.

**Quality:** [`docs/detection-report.json`](docs/detection-report.json) holds the precision, recall and false positive rate of each detector on a synthetic labeled corpus of 1,639 cases, with its SHA-256, the detector revision and the exact command. Regenerate it with `scripts/detection-report.ps1`, or on any system with:

```sh
go test ./internal/sensitivedata -run '^TestCorpusReport$' -count=1 -args -detection-report=../../docs/detection-report.json
```

The test suite fails when the published report no longer matches the corpus and the rules. On the current corpus every positive case is found; the false positive rates per document are 0 for cpf, cnpj and secret, 9.1% for card, 7.1% for email, 8.0% for phone and 12.5% for pix, all of them on the ambiguous negatives the corpus labels on purpose (order numbers that pass Luhn, protocol numbers shaped like phones, file names like `foto@2x.png`, UUIDs used as trace IDs). The corpus is synthetic and was written knowing the rules, so these numbers describe the rules, not production traffic.

**Performance:** on an Intel Core i5-9400F (6 cores) under Windows with Go 1.26.11, the scan alone takes about 0.9 ms on a 400 KB prompt without matches in detect mode, and about 7 ms on 400 KB packed with malformed or real candidates (`go test ./internal/sensitivedata -run XXX -bench .`). The end-to-end scenario in `tests/integration/sensitive_bench_test.go` sends the same 400,000-byte request, with one email and one CPF in it, sequentially to one gateway with every category `off` and then in `detect`, so detect also pays the synchronous commit of its event. Five repetitions of 1,000 requests per mode on that machine measured a p50 of about 17.5 ms off and 22.3 ms in detect, and p95 differences between -18 ms and +67 ms: the SQLite commits of that disk vary more than the scan costs. The worst p95 increase, 67 ms, is far from the 5 ms criterion, and this is not the reference benchmark, so the default stays `off`. Reproduce it with `COMPORTA_SENSITIVE_BENCH=1 go test ./tests/integration -run '^TestSensitiveLatencyScenario$' -count=1 -timeout 60m`; the defaults are five repetitions of 10,000 requests, and `COMPORTA_SENSITIVE_BENCH_OUT` writes the result as JSON.

### Alerts

Comporta tells you about spend and protection events through the channels you configure, outside the request path: a slow or broken receiver never delays, blocks or fails a request.

```yaml
alerts:
  thresholds: [50, 80, 100]      # default; budget percentages, strictly increasing
  anomaly_multiplier: 5          # default
  anomaly_min_usd: 0.50          # default
  channels:
    ops-webhook:
      type: webhook
      url: ${ALERT_WEBHOOK_URL}
      signing_secret_file: /run/secrets/alert-signing
    ops-discord:
      type: discord
      url: ${DISCORD_WEBHOOK_URL}
    ops-slack:
      type: slack
      url_file: /run/secrets/slack-webhook
    ops-ntfy:
      type: ntfy
      server_url: https://ntfy.example.com   # the public server or a self-hosted one
      topic: comporta-alerts
      token: ${NTFY_TOKEN}                   # optional
```

Every alert goes once to every channel. The triggers:

| Trigger | When | Deduplication |
|---|---|---|
| `budget_threshold` | The confirmed spend of an app budget, or of one user's per-user budget, reaches a threshold. A jump past several thresholds sends each of them | Once per app, scope, user, dimension, window and threshold |
| `hard_cap` | A request is refused by a hard cap | Once per app, scope, user, dimension and window |
| `anomaly` | The last hour's spend of an app is at least `anomaly_min_usd` and greater than `anomaly_multiplier` times its 7-day hourly average (hours without spend count as zero). Evaluated every 5 minutes | Once per app every 15 minutes |
| `loop_cut` | The loop guard cuts a user's requests | Once per app every 15 minutes |
| `fallback_burst` | At least 5 fallbacks of one app in 5 minutes | Once per app every 15 minutes |
| `config_reload_failed` | A changed configuration file is rejected | Once every 15 minutes |
| `fail_open` | Limit state cannot be verified for an app whose fail mode is `fail_open` | Once per app every 15 minutes |

Discord and Slack receive a formatted message such as `⚠️ bot-discord reached 80% of its daily budget ($1.60 of $2.00). Renews at 00:00.`, in the budget's timezone; ntfy receives `Comporta: loop cut in bot-discord (user u_9f2c4f…, 10 repeats in 30s)`. The generic webhook receives the JSON payload, signed: `X-Comporta-Signature` is `sha256=` followed by the HMAC-SHA256, in lowercase hex, of `X-Comporta-Timestamp`, a dot and the exact body. Receivers should compare in constant time, reject timestamps more than 5 minutes away and deduplicate on `X-Comporta-Delivery-ID`.

Alerts carry metadata only: app names, closed categories, amounts, times and, for user budgets and loop cuts, the user's pseudonym (abbreviated in chat messages). Never prompts, responses, raw user IDs, keys, URLs, configuration values or external error messages.

A delivery that fails with a timeout, a connection error, 408, 425, 429 or 5xx is retried three times, after at least 2, 8 and 32 seconds (or the receiver's `Retry-After`, up to 5 minutes); other answers, redirects included, fail at once. Alerts survive restarts: the evaluation queue and the deliveries live in the usage store, so a restart resumes pending retries and never repeats an alert already confirmed. Removing a channel, or changing its type, cancels its pending deliveries; a new URL or token is used from the next attempt on. See [docs/alerts.md](docs/alerts.md) for the payload, the verification of signatures, multiple instances and operation.

### End users

To attribute requests to the people using your app, send an opaque ID. In most SDKs that is one line, and it is the only code change later per-user limits will need:

```python
client = anthropic.Anthropic(base_url="http://127.0.0.1:8080", api_key=key, default_headers={"X-Comporta-User": user.id})
client = openai.OpenAI(base_url="http://127.0.0.1:8080/v1", api_key=key, default_headers={"X-Comporta-User": user.id})
```

The ID is read from `X-Comporta-User`, then `metadata.user_id`, then `user`. The first one present is used, on every format, and a present but empty value is refused instead of falling through to the next. The header goes no further than the gateway.

**What is accepted:** 1 to 128 characters of `A-Z a-z 0-9 _ . : -`, compared exactly: no trimming and no change of case. IDs shaped like an email address, or a valid CPF or CNPJ (numeric or alphanumeric, with or without the usual mask, check digits verified), are refused. A long ID that merely contains such a number is fine, and an opaque ID that happens to form a valid document is refused too. Only these fields are checked: an ID your app copies into a message or into another metadata field is content, and stays as it is.

| `comporta_code` | Message |
|---|---|
| `user_required` | `this app requires a user ID (X-Comporta-User header)` |
| `user_invalid` | `user ID must be 1-128 characters of [A-Za-z0-9_.:-]` |
| `user_email_not_allowed` | `user ID looks like an email address; send an opaque ID instead` |
| `user_document_not_allowed` | `user ID looks like a CPF or CNPJ; send an opaque ID instead` |

**Without an ID:** an app with `require_user: true` gets `user_required`, read from the configuration in force when the request arrived. Otherwise the request counts as the app's anonymous user, kept apart from a user whose ID is literally `anonymous`. `GET /v1/models` never asks for a user.

**What the provider sees:** a pseudonym of 32 hexadecimal characters, HMAC-SHA256 truncated to 16 bytes, stable across restarts and app key rotation. The same person in two apps gets two different pseudonyms, so renaming an app also breaks that app's continuity. Every known identity field in the body is replaced, including ones that lost the precedence, and the native field is added when missing: `metadata.user_id` on `/v1/messages`, which makes a `metadata` that is not an object a 400, and `user` on `/v1/chat/completions`. `count_tokens` replaces what is present and adds nothing. The raw ID is never logged or stored.

**What this does not prove:** Comporta authenticates the app, not its users. The app decides which ID goes with which person and can send any ID it likes, so a pseudonym is only as trustworthy as the app that sent it.

### The identity key

`serve` loads `hmac.key` from the data directory before opening any port, and creates it on the first run. It holds 32 random bytes and follows the same [file protections](#files) as the app keys. A file of any other size or with broader access stops the service: Comporta never repairs or replaces it. There is no configuration for it, and changes to the file only apply after a restart.

**Back it up together with the data directory, and keep it private.** Anyone with the key and a list of candidate IDs can recognize pseudonyms. Replacing or losing the key breaks the continuity of per-user history, because the same user gets a new pseudonym, and there is no migration. To rotate it, stop the service and plan for that break. App keys are independent, so rotating them changes nothing here.

When the key is missing, Comporta inspects logical SQLite history, including WAL, pending admissions, per-user aggregates and limit state, before creating a database or migrating it. Unknown files, unsupported schemas and unreadable history fail closed. Existing user history stops startup with `hmac.key not found but usage data exists; restore the key or start with --reset-user-history`. Restore the key from the same backup as the database whenever possible.

With the service stopped, `serve --reset-user-history` authorizes logical unlinking after a lost key. Pending attempts are recovered first; a transaction removes identifiable details and user totals/state while preserving app totals and app budget state. The reset is idempotent across a crash before publishing the new key. It does not transfer old users into anonymous or change app keys. A valid existing HMAC key is never replaced. Unknown history is still refused. Logical unlinking is not forensic erasure of old database pages, backups or media.

## Operations

The admin API lives only on the admin listener (`127.0.0.1:8081` by default), under `/admin/v1`. It does not exist on the proxy port.

### Admin token

On the first start `serve` creates a 32-byte random token and prints it once on stdout: store it then. The data directory keeps only its SHA-256 in `admin-token.json`, with the same private permissions as the other files, and a restart never prints it again. While running, the gateway holds `admin-token.lock`.

If the token is lost, stop the gateway and run `comporta admin reset-token --config comporta.yaml`: it prints a new token once and keeps pauses, usage and the user identity key. It refuses while the gateway runs. A token file that disappears or is corrupted after the first start stops the gateway at startup until a reset; it is never replaced silently.

CLI commands send the token as `Authorization: Bearer`, from `--token` or `COMPORTA_ADMIN_TOKEN` (the flag wins). The destination is the admin listener of `--config` (a wildcard bind is reached on `127.0.0.1`) or `--admin-url`. Plain HTTP is accepted only for a loopback address; a remote gateway needs HTTPS with standard certificate validation. Redirects and proxy environment variables are ignored. A missing or wrong token answers 401 `invalid admin token`; the tenth failure from one address within 60 seconds blocks that address for 5 minutes, with 429 and `Retry-After`, even for a valid token.

### Pause and resume

```sh
$ comporta pause bot-discord
bot-discord paused at 03:12:44; resume with: comporta resume bot-discord
```

When the command returns, every new request of the app is refused with 503 `app bot-discord is paused by an operator` (`comporta_code: app_paused`), in the SDK error format and without contacting the provider. Requests already admitted, streams included, finish normally. `GET /v1/models` keeps answering. A pause consumes no budget.

`pause --all` pauses every app, including apps added later by a reload. `resume --all` clears the global pause and every individual one. Resuming one app while the global pause is on says that the app remains paused. Pauses are stored in `runtime_state`, survive restarts and reloads, and are the one documented exception to "YAML is the source of truth". The pause of a removed app stays stored and can be cleared with `resume`. Each change writes a `paused` event with `paused: true` or `false`.

### Deleting an end user

```sh
$ comporta users forget user_42
deleted 16 rows; unlinked 4 rows kept for app totals; cleared 2 in-memory counters
```

The ID is pseudonymized for every app with any state, including apps removed from the YAML, and is never stored or logged by the gateway (it stays in your shell history). New requests of that user get 503 `user_maintenance` for the duration; requests already running are given up to 10 seconds to finish and reach accounting, otherwise the command fails with 409 and deletes nothing. Then one transaction deletes the user's request records, daily totals, events, user budgets and reservation items, and unlinks the user from reservations, rate events, the limit ledger and the checkpoint. The response cache of every app involved is emptied as well. App totals, app budgets and app rate limits do not change. A locked database is retried for 10 seconds; after that the answer is `deletion failed; no records were removed`. A user without records gets `no records found for this user ID` with exit code 0. `anonymous` names a user called anonymous, never the anonymous bucket.

Deletion is logical: it does not overwrite old SQLite pages, backups or disks. It does not ban the user: new traffic creates new history. If the connection drops after the request was sent, the CLI reports an unknown outcome; the command is safe to repeat.

### Usage

`comporta usage` needs the running gateway and the token: it no longer reads the database directly. Without `--app` it covers every app, including removed ones. `--user` and `--anonymous` select only that user's rows and are mutually exclusive; without them the app totals are shown. `--since Nd` (default `7d`) counts civil days back from now in each app's timezone, or the historical timezone of a removed app. `--from` and `--through` select recorded day labels, inclusive. A period covers at most 366 days. `--json` prints the full result.

### Admin API

Requests and answers are JSON (at most 16 KiB, `Content-Type: application/json`, unknown fields rejected). Answers carry `Cache-Control: no-store`. Failures use `{"error":{"code":"...","message":"..."}}`.

| Route | Body | Answer |
|---|---|---|
| `POST /admin/v1/session` | `{"token":"..."}` | 201 and an `HttpOnly`, `SameSite=Strict` session cookie for 12 hours |
| `DELETE /admin/v1/session` | none | 204, session ended |
| `GET /admin/v1/pause` | none | global state and the stored pauses |
| `POST /admin/v1/pause`, `POST /admin/v1/resume` | `{"app":"x"}` or `{"all":true}` | effective state |
| `POST /admin/v1/users/forget` | `{"user_id":"..."}` | row counts, without the ID |
| `POST /admin/v1/usage/query` | `app`, `user_id` or `anonymous`, `since` or `from`+`through`, `limit`, `cursor` | rows, totals of the whole filter and `next_cursor` |

Requests authenticated with the session cookie must send a matching `Origin` on every POST and DELETE. IDs never go in a URL. Counts and money in usage answers are decimal strings. Usage cursors are signed with a per-process key and stop working after a restart. Sessions live in memory and end on restart.

The admin panel below logs in through the same credential, sessions and address guard as these routes.

## Admin panel

The panel is served by the same binary on the admin listener, at `http://127.0.0.1:8081/` by default. It is read only: no screen changes the configuration, pauses an app or deletes a user. It does not exist on the proxy port.

Open `/login` and paste the admin token. The form posts it in the body, never in a URL, under the same guard as the API (ten failures from one address in 60 seconds block it for 5 minutes). A valid token becomes the usual session cookie (`HttpOnly`, `SameSite=Strict`, `Secure` over TLS, 12 hours, never renewed) and the token is never shown back. A bearer token does not open pages. Log out with the button in the header.

| Screen | Shows |
|---|---|
| Overview | Estimated cost today and this month, requests and tokens today, one bar per app against its daily and monthly budget, estimated savings from cache and downgrade, the last 10 alerts and a red banner for paused apps or the global pause |
| Apps | Every app with its policy, main model, cost and tightest limit; per app, each limit with used, reserved, ceiling and `renews in`, the active and pending timezone, the model mix and the top 10 user pseudonyms by cost this month |
| Users | A search box that hashes the typed ID for every app, current or removed, and shows the pseudonym, today's cost, each per-user limit and the month's history. The anonymous bucket has its own control |
| Requests | The latest 500 attempts, newest first, 50 per page, filtered by app, user, model and outcome (`ok`, `blocked`, `cached`, `fallback`, `downgraded`, `loop_cut`); a row opens its metadata |
| Events | Loop cuts, fallbacks, blocks, downgrades, warnings, cache hits, configuration changes, pauses and limit degradation, newest first, 50 per page |
| Providers | Per provider over the last hour: health, attempts, error rate, p50 and p95 latency, prompt cache read ratio and a five-minute series |
| Configuration | The effective YAML with secrets shown as `[REDACTED]`, the active generation and the last reload, with its errors highlighted when it was rejected |

What the numbers mean:

- Days and months follow each app's timezone; totals add each app's own window.
- Costs are estimates in dollars from the price catalog, never an invoice. A request recovered after a crash or without usage from the provider is marked and counts its conservative reservation.
- Savings are potential and estimated: a cache hit saves the reservation it released, a downgrade saves the reservation of the requested model minus the one sent. Both are frozen when the request is decided and counted once per logical request. History written before this version counts as zero.
- Provider health is observed from traffic, not from an active check: no attempt in the hour is `unknown`, under 5% of overload, timeout, connection and 5xx failures is `healthy`, up to 20% is `degraded`, above that or three attempts without a success is `unhealthy`. Client errors do not lower it. The ratio is `cache_read / (input + cache_read)`.
- Limit state that cannot be proven shows as not verifiable, never as free budget. With several replicas limits live in the shared stores and the panel shows them that way.
- Users appear only as an app-scoped pseudonym such as `u_9f2c41ab`. The typed ID stays in the search field, is posted in the body and is never returned, stored, logged or put in a URL. Prompts, responses, headers and raw provider errors are never stored, so the panel cannot show them.

Screens update live. One stream per browser (`/panel/live`, at most 128 per process) carries only which screens are stale, at most once per second; the browser then fetches the fragment. Filters are kept, and a list you are reading shows "New data available" instead of moving. New data can take up to two seconds to appear. With several replicas each process also follows a shared watermark once per second, so a write on another replica refreshes the screen too. A full stream answers 503 and manual reload keeps working. When the session expires the stream says so and the page goes to the login form.

Everything the browser loads is embedded in the binary: HTML rendered on the server with [templ](https://templ.guide), [HTMX](https://htmx.org) 2.0.11 with its SSE extension 2.2.4, [uPlot](https://github.com/leeoniya/uPlot) 1.6.32 and a stylesheet built with the Tailwind CSS standalone CLI 4.3.5. No CDN, Node.js or network access is needed to build or run. Pages send a Content-Security-Policy without `unsafe-inline` or `unsafe-eval`, `X-Frame-Options: DENY`, `Referrer-Policy: no-referrer` and `Cache-Control: no-store`; HTMX runs with eval, script tags in content and the history cache disabled. Charts have an equivalent table, and the panel stays usable without JavaScript, with manual reload.

Limitations: sessions of a single instance live in memory and end on restart; the panel has no accounts or roles; reaching it from another machine needs the admin listener exposed on purpose, which is not recommended.

## Observability

`GET /metrics` on the admin listener exposes Prometheus metrics behind the admin token: requests by app, model and outcome, latency and TTFB, upstream attempts, estimated cost and tokens, blocks, cache hits, fallbacks and budget ratios. With `telemetry.otel_enabled`, each proxy request becomes one OpenTelemetry trace with a span per pipeline stage, sent over OTLP/HTTP. Neither carries content, user IDs or user hashes, and both follow reloads.

```sh
curl -H "Authorization: Bearer $COMPORTA_ADMIN_TOKEN" http://127.0.0.1:8081/metrics
```

`docker compose --profile observability up` adds Prometheus, Grafana with the Comporta dashboard and Jaeger, after a short preparation of the admin token and a Grafana password in private files. See [docs/observability.md](docs/observability.md) for the settings, the metric catalog, the trace structure, the preparation and the limits.

## Fake provider

`comporta fake-provider` imitates both APIs with synthetic, deterministic responses, for tests without keys or cost. It listens on `127.0.0.1:9090` by default. The scenario is chosen when it starts, and no request can change it: `success`, `rate-limit` (429), `overload` (529), `server-error` (500), `bad-request` (400), `header-timeout`, `slow-stream`, `disconnect-midstream`, `stream-error-before-chunk` (an overload error as the first stream event, 529 without streaming), `stream-error-after-chunk` (the same error after one event) and `agent-loop`, which answers every conversation with the same tool call, so a client that keeps sending back the same tool result reproduces an agent loop with growing context. `--model-scenario MODEL=SCENARIO`, repeatable, overrides the scenario for one model, so one fake plays a whole fallback chain: `comporta fake-provider --model-scenario fake-claude=overload` overloads `fake-claude` and answers every other model.

```yaml
providers:
  fake:
    api_format: anthropic
    base_url: http://127.0.0.1:9090
    models: [fake-claude]
pricing:
  fake-claude: {input: 2, output: 6, cache_write: 2.5, cache_read: 0.2}
```

## Multiple replicas

Before this release limits lived in the memory of one process, so two replicas would each grant the full budget and a 10 USD daily budget would become 20. A replica count above 1 with SQLite still aborts at start with `SQLite storage does not support multiple replicas; configure redis and postgres`.

With `deployment.mode: distributed`, replicas share budgets, rate limits, concurrency, pauses, user deletions, loop guard counts, app keys and the admin credential through Postgres (durable journal, ledger and history) and a standalone Redis (the projection admissions decide on). The response cache stays local to each replica. Nothing is sent upstream before an admission is durable in Postgres, so the overshoot guarantee of a single instance holds across replicas under `fail_closed`. Redis unavailable refuses with 503 under `fail_closed`; under `fail_open` the cluster runs degraded and may exceed budgets until Redis is back and the spend is reconciled. Postgres unavailable refuses new requests at once, while completions already admitted wait in memory.

```sh
comporta storage migrate --config comporta.yaml
comporta admin reset-token --config comporta.yaml
comporta keys create my-app --config comporta.yaml
comporta serve --config comporta.yaml   # on every replica
helm install comporta ./deploy/helm -f examples/kubernetes/values.yaml
```

[`examples/distributed.yaml`](examples/distributed.yaml) is a complete configuration and [`deploy/helm`](deploy/helm) the official chart: two or more replicas, non-root, read-only root filesystem, no capabilities, secrets mounted as files, network policies and readiness on `/readyz`. There is no automatic migration of an existing SQLite installation: a distributed cluster starts with empty budgets. See [docs/multi-instance.md](docs/multi-instance.md) for requirements, failures, recovery, rollouts and cutover.

## Container

The image is distroless and runs as a non-root user. `compose.yaml` sets a read-only root filesystem, drops all capabilities, allows writes only to the `/data` volume and publishes both ports on `127.0.0.1` only. Inside the container the service binds `0.0.0.0`, as [`examples/comporta-container.yaml`](examples/comporta-container.yaml) shows.

```sh
export ANTHROPIC_API_KEY=your-key   # or put it in a .env file next to compose.yaml
docker compose build
docker compose run --rm comporta keys create my-app --config /etc/comporta/comporta.yaml
docker compose up -d
curl http://127.0.0.1:8081/healthz
```

App keys, `hmac.key` and SQLite DB/WAL/SHM live in the volume and survive recreating the container. Stop the service before copying a backup, or use a consistent SQLite backup that includes WAL, and keep the matching identity key. Allow time for the accounting flush before stopping the container. `COMPORTA_CONFIG` selects another configuration file, which is mounted read-only. The image has no shell or curl, so check it from the host. Reaching the service from another machine means publishing the proxy port elsewhere and adding its name to `allowed_hosts`; never publish the admin port that way.

## Development

The repository uses [Task](https://taskfile.dev). `task --list` shows the available tasks, and `task check` runs formatting, static checks, tests, example validation and the vulnerability and secret scanners. The panel's generated files are committed: after changing a `.templ` file or the stylesheet run `task panel-assets`, which uses templ 0.3.906 through `go run` and downloads the Tailwind CSS standalone CLI 4.3.5 once into `bin/`, checking it against the checksum of its release; `task panel-check` fails when the committed output differs. The race detector needs a C toolchain: `task test-race`. `task test-privacy` runs the privacy test, `task test-sdk` runs the official Python SDKs at the versions pinned in `tests/sdk/requirements.txt`, and `task test-container` builds the image and verifies its restrictions, `task test-distributed` runs the shared stores and two `serve` processes against real Postgres and Redis containers, `task chart` checks the Helm chart and `task test-kubernetes` installs it on a throwaway kind cluster; none of them needs provider credentials. A test that adds a prompt builds it with `internal/testkit`, or the marked `prompt` helper in the SDK suite, and new destinations for data go in `tests/privacy/coverage.json` in the same change.

`go test ./tests/integration -run '^$' -bench '^BenchmarkHTTPMetering$' -benchtime 500x` compares real HTTP and streaming latency with and without durable accounting and reports p95/TTFB. Sync latency depends on the deployment filesystem, so use low-latency local storage and measure there. This diagnostic does not certify the sustained 200 requests/second, five-minute load target.
