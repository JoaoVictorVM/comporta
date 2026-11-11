# Comporta

Open-source LLM gateway, in development.

**Current state:** this repository delivers the configuration runtime, the app key lifecycle, the request proxy, usage metering, per-app and per-user limits and budgets, and operator commands (pause, resume, user deletion and usage queries) through an authenticated admin API. `comporta serve` opens the proxy and admin listeners and forwards SDK traffic to the configured providers, with authentication, streaming, hot reload and a built-in fake provider.

**Not implemented yet:** policies (including `max_tokens` caps and `allowed_models` enforcement on calls), fallback, cache, loop guard, sensitive data detection, alerts, telemetry and the admin panel. Their settings are validated and made available **without taking effect**, so do not rely on them as protection.

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
- **Hard caps:** with `on_limit: downgrade`, an omitted dimension defaults to 120% of its budget, rounded up. With `on_limit: warn`, an omitted dimension has no hard ceiling.

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
| `GET /v1/models` | Chosen by the client | Local list, see below |

Every other path or method returns 404 `endpoint not allowed by Comporta`, without forwarding anything, so an app cannot use your provider key for files, batches or admin APIs. The admin listener has only `GET /healthz`, which answers `{"status":"ok"}` and says nothing about providers.

**What reaches the provider:** the body exactly as received, byte for byte, including unknown fields, with documented patches. End-user fields are replaced by a pseudonym, as [End users](#end-users) describes. On OpenAI-format streaming, `stream_options.include_usage` is set to `true` so that usage can be measured, and if the client did not ask for it, the extra usage chunk is removed before delivery. Generation output is capped by the app's `max_tokens`: a higher value is reduced and an absent OpenAI limit is inserted. An existing `max_completion_tokens` field keeps its name. This cap makes the admission quote bounded before the remaining policy features are available. Only `content-type`, `anthropic-version` and `anthropic-beta` are forwarded; every other client header is dropped, and the app key is replaced by the provider key. The destination comes only from the configuration. Redirects are never followed, proxy environment variables are ignored, and a remote provider name that resolves to a private, loopback or link-local address is refused. A base URL may end in `/v1` or not; the same path results.

**Errors** use the format of the route and carry a `comporta_code`. Provider error messages are never passed on, because they can echo prompts: status and category are preserved and the message is rebuilt. Inside a stream already started, the failure is an error event followed by closing the stream, so a truncated stream never looks like a success.

| `comporta_code` | Status | Meaning |
|---|---|---|
| `endpoint_not_allowed` | 404 | Route or method outside the list |
| `host_not_allowed` | 421 | `Host` not allowed |
| `invalid_request` | 400 | Invalid JSON, types, duplicate keys or query |
| `user_required`, `user_invalid`, `user_email_not_allowed`, `user_document_not_allowed` | 400 | End-user identifier missing or refused, see [End users](#end-users) |
| `model_unknown`, `provider_format_mismatch` | 400 | Model not declared, or declared in another API format |
| `price_unknown`, `pricing_overflow` | 400 | No effective price, or cost outside the supported integer range |
| `rate_limited`, `concurrency_exceeded`, `budget_exceeded`, `budget_reserved`, `hard_cap_exceeded` | 429 | A limit or budget, see [Limits and budgets](#limits-and-budgets) |
| `limits_unavailable` | 503 | Limit state cannot be verified under `fail_closed` |
| `usage_storage_unavailable` | 503 | No accounting capacity, or durable admission could not be confirmed; provider is not called |
| `unsupported_media_type` | 415 | Body is not uncompressed JSON |
| `request_too_large` | 413 | Body over `max_body_bytes` |
| `provider_unreachable`, `provider_redirect`, `invalid_upstream_response` | 502 | Provider could not be used |
| `provider_timeout` | 504 | A deadline expired |
| `provider_rate_limited`, `provider_overloaded`, `provider_error` | Provider's | Provider refused or failed |
| `internal_error` | 500 | A panic, limited to that request and logged without content |

**Limits:** request body 10 MB, response headers from the provider 60 s, non-streaming requests 300 s in total including reading the body, stream silence 60 s, all configurable under `listeners`. Fixed: 64 MB per non-streaming response, 10 MB per stream event, JSON nesting of 128 levels, 64 KiB of request headers. On shutdown, requests get 10 s to finish and are then canceled, so a long stream can end when the service stops.

**Duplicate JSON keys are refused** anywhere in the body, because the gateway and the provider could otherwise read different values. Each client connection carries one request, which is what lets a reload change the header timeout, so clients open a new connection per request. Timeouts and allowed hosts apply to new requests after a reload; requests in flight keep the configuration and identity they started with. Connections to providers use HTTP/1.1.

### Model catalog

`GET /v1/models` lists the app's `allowed_models` in the format of the client, without calling a provider, so it costs nothing and says nothing about availability. `created` and `created_at` hold fixed values because the configuration has no release dates. The Anthropic format supports `limit`, `after_id` and `before_id`. This list is only a listing: until policies exist, an app can still call any declared model.

### Usage and privacy

Each provider call produces a private SQLite record with counters, timing, categories and the user's app-scoped pseudonym, never content, raw user identifiers, request digests or raw errors. A counter the provider did not report stays unknown. Observed counters are preserved even when the final cost must use a conservative estimate. `count_tokens` has its own operation and advisory counter, with zero generation cost and zero consumption tokens. Listing models is free and creates no consumption.

Prompts and responses stay in memory for the duration of the request and are not logged, and no option enables logging them. A privacy test sends marked prompts and identifiers through the gateway, including error paths, and fails if a marker appears in logs, answers built by the gateway, results, database artifacts or generated files. It requires nonempty persisted usage, aggregates and events and scans the DB/WAL before and after recovery. [`tests/privacy/coverage.json`](tests/privacy/coverage.json) tracks these channels and the future metrics, traces and alert consumers.

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
- **Policies:** this release always blocks. An `on_limit` of `downgrade` or `warn` is accepted by the configuration and logged as not implemented yet; it does not downgrade or warn.

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

**Fail modes:** with `fail_closed`, the default, a call is refused with 503 when limit state cannot be read or written, for example when a checkpoint fails or a failed settlement also fails its retry. With `fail_open` the call proceeds, its record is marked degraded, limits are still evaluated from memory, and a `limit_state_unavailable` event (severity warning, per app) and a log warning are emitted; `limit_state_recovered` follows once state is persisted again. `fail_open` never bypasses the usage journal: if usage cannot be admitted durably the call gets 503 `usage_storage_unavailable`. Arithmetic overflow and integrity failures never fail open. Delivery of these events to Slack or webhooks belongs to the alerts feature.

**Guarantee and its limits:** admissions are serialized with their reservations, so when real usage stays within the reservation, concurrent calls cannot overrun a budget. A test fires 500 simultaneous calls at one budget in 100 consecutive rounds against SQLite and the fake provider and checks that spend never exceeds the budget by more than the largest single reservation; on the reference Windows machine every round stayed within the budget. When a provider reports more usage than estimated, the real cost is charged and the budget then refuses new calls, but several calls in flight that all exceed their estimate can together exceed the budget by more than one reservation; a test demonstrates this case. `fail_open` also suspends the guarantee while degraded. Token estimation is a heuristic, not a tokenizer.

**Measured cost:** admission and settlement reuse the two synchronous `synchronous=FULL` commits that metering already made, now carrying the reservation. On the reference Windows machine, sequential non-streaming calls through the fake provider measured a p95 of 6.7 ms against 2.0 ms without metering and limits, and streaming time to first byte a p95 of 3.6 ms against 2.6 ms. The coordinator alone takes about 27 µs per reserve and settle. The sustained load target of 200 requests per second for 5 minutes was not measured in this release.

**For later features:** policies, the panel, alerts, telemetry, fallback, cache and multiple instances consume typed decisions, a consistent state per app and user (used, reserved, limit, renewal, degradation, active and pending timezone) and a reservation handle with reserve, settle and release. `comporta users forget` removes a user's limit state together with the user's records, see Operations.

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

The ID is pseudonymized for every app with any state, including apps removed from the YAML, and is never stored or logged by the gateway (it stays in your shell history). New requests of that user get 503 `user_maintenance` for the duration; requests already running are given up to 10 seconds to finish and reach accounting, otherwise the command fails with 409 and deletes nothing. Then one transaction deletes the user's request records, daily totals, events, user budgets and reservation items, and unlinks the user from reservations, rate events, the limit ledger and the checkpoint. App totals, app budgets and app rate limits do not change. A locked database is retried for 10 seconds; after that the answer is `deletion failed; no records were removed`. A user without records gets `no records found for this user ID` with exit code 0. `anonymous` names a user called anonymous, never the anonymous bucket.

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

These routes are what the future panel will use. Pauses and sessions are local to one gateway process: keeping them consistent across several instances belongs to multi-instance support, not delivered yet.

## Fake provider

`comporta fake-provider` imitates both APIs with synthetic, deterministic responses, for tests without keys or cost. It listens on `127.0.0.1:9090` by default. The scenario is chosen when it starts, and no request can change it: `success`, `rate-limit` (429), `overload` (529), `header-timeout`, `slow-stream` and `disconnect-midstream`.

```yaml
providers:
  fake:
    api_format: anthropic
    base_url: http://127.0.0.1:9090
    models: [fake-claude]
pricing:
  fake-claude: {input: 2, output: 6, cache_write: 2.5, cache_read: 0.2}
```

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

The repository uses [Task](https://taskfile.dev). `task --list` shows the available tasks, and `task check` runs formatting, static checks, tests, example validation and the vulnerability and secret scanners. The race detector needs a C toolchain: `task test-race`. `task test-privacy` runs the privacy test, `task test-sdk` runs the official Python SDKs at the versions pinned in `tests/sdk/requirements.txt`, and `task test-container` builds the image and verifies its restrictions; none of them needs provider credentials. A test that adds a prompt builds it with `internal/testkit`, or the marked `prompt` helper in the SDK suite, and new destinations for data go in `tests/privacy/coverage.json` in the same change.

`go test ./tests/integration -run '^$' -bench '^BenchmarkHTTPMetering$' -benchtime 500x` compares real HTTP and streaming latency with and without durable accounting and reports p95/TTFB. Sync latency depends on the deployment filesystem, so use low-latency local storage and measure there. This diagnostic does not certify the sustained 200 requests/second, five-minute load target.
